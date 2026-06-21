package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	edgev1alpha1 "github.com/sertayy/sif-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	mqttTelemetryRefreshInterval = 15 * time.Second
	mqttTelemetryReadTimeout     = 5 * time.Second
	mqttTelemetryPingInterval    = 20 * time.Second
)

var errNoTelemetryTopics = errors.New("no telemetry topics configured")

type mqttTelemetryBridge struct {
	client       client.Client
	nextPacketID uint16
}

type batteryTelemetry struct {
	BatteryPercent *int32 `json:"batteryPercent"`
	Mode           string `json:"mode,omitempty"`
	Source         string `json:"source,omitempty"`
	VoltageMV      *int32 `json:"voltageMv,omitempty"`
	ArtifactDigest string `json:"artifactDigest,omitempty"`
}

type telemetryUpdateResult struct {
	statusChanged  bool
	reportableInfo bool
}

func NewMQTTTelemetryBridge(kubeClient client.Client) *mqttTelemetryBridge {
	return &mqttTelemetryBridge{client: kubeClient}
}

func (b *mqttTelemetryBridge) NeedLeaderElection() bool {
	return true
}

func telemetryTopicForDevice(device edgev1alpha1.DeviceSpec) string {
	if device.TelemetryTopic != "" {
		return strings.TrimSpace(device.TelemetryTopic)
	}
	if device.ControlTopic == "" {
		return ""
	}
	return strings.TrimRight(device.ControlTopic, "/") + "/telemetry"
}

func (b *mqttTelemetryBridge) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("mqtt-telemetry")
	cfg := mqttPublishConfig{
		Broker:   mqttBrokerAddress(),
		Username: os.Getenv("SIF_MQTT_USER"),
		Password: os.Getenv("SIF_MQTT_TOKEN"),
		ClientID: os.Getenv("SIF_MQTT_CLIENT_ID"),
	}
	if cfg.Username == "" {
		cfg.Username = "JWT"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = fmt.Sprintf("sif-operator-telemetry-%d", time.Now().UnixNano())
	}

	for {
		if ctx.Err() != nil {
			return nil
		}

		conn, subscribed, err := b.connectAndSubscribe(ctx, cfg)
		if err != nil {
			if !errors.Is(err, errNoTelemetryTopics) {
				log.Error(err, "Failed to connect MQTT telemetry bridge")
			}
			if !waitForContext(ctx, 5*time.Second) {
				return nil
			}
			continue
		}

		log.Info("MQTT telemetry bridge connected", "topics", sortedTopicKeys(subscribed))
		err = b.consumeTelemetry(ctx, conn, subscribed)
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Error(err, "MQTT telemetry bridge disconnected")
		}
		if !waitForContext(ctx, 2*time.Second) {
			return nil
		}
	}
}

func (b *mqttTelemetryBridge) connectAndSubscribe(ctx context.Context, cfg mqttPublishConfig) (net.Conn, map[string]struct{}, error) {
	topics, err := b.telemetryTopics(ctx)
	if err != nil {
		return nil, nil, err
	}

	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Broker)
	if err != nil {
		return nil, nil, fmt.Errorf("connect MQTT broker %s: %w", cfg.Broker, err)
	}

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err := mqttConnect(conn, cfg); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := b.subscribe(conn, topics); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	subscribed := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		subscribed[topic] = struct{}{}
	}
	return conn, subscribed, nil
}

func (b *mqttTelemetryBridge) consumeTelemetry(ctx context.Context, conn net.Conn, subscribed map[string]struct{}) error {
	log := ctrl.LoggerFrom(ctx).WithName("mqtt-telemetry")
	lastRefresh := time.Time{}
	lastClientWrite := time.Now()

	for {
		if ctx.Err() != nil {
			return nil
		}

		if time.Since(lastRefresh) >= mqttTelemetryRefreshInterval {
			topics, err := b.telemetryTopics(ctx)
			if err != nil {
				if !errors.Is(err, errNoTelemetryTopics) {
					log.Error(err, "Failed to refresh telemetry topics")
				}
			} else {
				missing := missingTopics(topics, subscribed)
				if len(missing) > 0 {
					if err := b.subscribe(conn, missing); err != nil {
						return err
					}
					lastClientWrite = time.Now()
					for _, topic := range missing {
						subscribed[topic] = struct{}{}
					}
					log.Info("Subscribed to new MQTT telemetry topics", "topics", missing)
				}
			}
			lastRefresh = time.Now()
		}

		_ = conn.SetReadDeadline(time.Now().Add(mqttTelemetryReadTimeout))
		header, packet, err := mqttReadPacket(conn)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if shouldSendTelemetryPing(lastClientWrite, time.Now()) {
					if err := mqttPing(conn); err != nil {
						return err
					}
					lastClientWrite = time.Now()
				}
				continue
			}
			return err
		}

		switch header & 0xF0 {
		case 0x30:
			topic, payload, err := mqttParsePublish(header, packet)
			if err != nil {
				log.Error(err, "Failed to decode MQTT publish packet")
				continue
			}
			if _, ok := subscribed[topic]; !ok {
				continue
			}
			if err := b.applyBatteryTelemetry(ctx, topic, payload); err != nil {
				log.Error(err, "Failed to apply MQTT telemetry", "topic", topic)
			}
		case 0xD0:
			continue
		default:
			continue
		}
	}
}

func (b *mqttTelemetryBridge) telemetryTopics(ctx context.Context) ([]string, error) {
	var list edgev1alpha1.WasmFunctionList
	if err := b.client.List(ctx, &list); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for _, wf := range list.Items {
		if topic := telemetryTopicForDevice(wf.Spec.Device); topic != "" {
			seen[topic] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, errNoTelemetryTopics
	}

	topics := make([]string, 0, len(seen))
	for topic := range seen {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	return topics, nil
}

func (b *mqttTelemetryBridge) applyBatteryTelemetry(ctx context.Context, topic string, payload []byte) error {
	log := ctrl.LoggerFrom(ctx).WithName("mqtt-telemetry")

	var telemetry batteryTelemetry
	if err := json.Unmarshal(payload, &telemetry); err != nil {
		return fmt.Errorf("decode telemetry payload: %w", err)
	}
	if telemetry.BatteryPercent == nil {
		return errors.New("telemetry payload missing batteryPercent")
	}

	battery := *telemetry.BatteryPercent
	if battery < 0 {
		battery = 0
	}
	if battery > 100 {
		battery = 100
	}
	telemetry.Mode = strings.ToLower(strings.TrimSpace(telemetry.Mode))
	telemetry.Source = strings.ToLower(strings.TrimSpace(telemetry.Source))
	telemetry.ArtifactDigest = normalizeArtifactDigest(telemetry.ArtifactDigest)
	if telemetry.VoltageMV != nil && *telemetry.VoltageMV < 0 {
		zero := int32(0)
		telemetry.VoltageMV = &zero
	}

	var list edgev1alpha1.WasmFunctionList
	if err := b.client.List(ctx, &list); err != nil {
		return err
	}

	for _, wf := range list.Items {
		if telemetryTopicForDevice(wf.Spec.Device) != topic {
			continue
		}
		key := types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}
		result, err := b.updateObservedTelemetry(ctx, key, telemetry, battery)
		if err != nil {
			return err
		}
		if result.reportableInfo {
			log.Info("Applied MQTT telemetry",
				"namespace", key.Namespace,
				"name", key.Name,
				"topic", topic,
				"batteryPercent", battery,
				"mode", telemetry.Mode,
				"batterySource", telemetry.Source,
				"artifactDigest", telemetry.ArtifactDigest)
		}
	}
	return nil
}

func normalizeArtifactDigest(digest string) string {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != 64 {
		return ""
	}
	for _, ch := range digest {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return digest
}

func (b *mqttTelemetryBridge) updateObservedTelemetry(ctx context.Context, key types.NamespacedName, telemetry batteryTelemetry, battery int32) (telemetryUpdateResult, error) {
	result := telemetryUpdateResult{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var wf edgev1alpha1.WasmFunction
		if err := b.client.Get(ctx, key, &wf); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		base := wf.DeepCopy()
		observed := battery
		if wf.Status.ObservedBatteryPercent == nil ||
			*wf.Status.ObservedBatteryPercent != battery {
			wf.Status.ObservedBatteryPercent = &observed
			result.statusChanged = true
		}
		if telemetry.Mode != "" && wf.Status.ObservedMode != telemetry.Mode {
			wf.Status.ObservedMode = telemetry.Mode
			result.statusChanged = true
			result.reportableInfo = true
		}
		if telemetry.Source != "" && wf.Status.ObservedBatterySource != telemetry.Source {
			wf.Status.ObservedBatterySource = telemetry.Source
			result.statusChanged = true
			result.reportableInfo = true
		}
		if telemetry.VoltageMV != nil {
			voltage := *telemetry.VoltageMV
			if wf.Status.ObservedVoltageMillivolts == nil ||
				*wf.Status.ObservedVoltageMillivolts != voltage {
				wf.Status.ObservedVoltageMillivolts = &voltage
				result.statusChanged = true
			}
		}
		if telemetry.ArtifactDigest != "" &&
			wf.Status.ObservedArtifactDigest != telemetry.ArtifactDigest {
			wf.Status.ObservedArtifactDigest = telemetry.ArtifactDigest
			result.statusChanged = true
			result.reportableInfo = true
		}
		now := metav1.Now()
		wf.Status.LastTelemetryTime = &now
		return b.client.Status().Patch(ctx, &wf, client.MergeFrom(base))
	})
	return result, err
}

func (b *mqttTelemetryBridge) subscribe(conn net.Conn, topics []string) error {
	if len(topics) == 0 {
		return nil
	}

	packetID := b.nextSubscribePacketID()
	variableHeader := make([]byte, 2)
	binary.BigEndian.PutUint16(variableHeader, packetID)

	payload := make([]byte, 0)
	for _, topic := range topics {
		payload = append(payload, mqttString(topic)...)
		payload = append(payload, 0x00)
	}

	packet := mqttFixedHeader(0x82, len(variableHeader)+len(payload))
	packet = append(packet, variableHeader...)
	packet = append(packet, payload...)
	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("write MQTT SUBSCRIBE: %w", err)
	}

	header, ack, err := mqttReadPacket(conn)
	if err != nil {
		return fmt.Errorf("read MQTT SUBACK: %w", err)
	}
	if header != 0x90 {
		return fmt.Errorf("unexpected MQTT packet 0x%x while waiting for SUBACK", header)
	}
	if len(ack) < 2 {
		return errors.New("short MQTT SUBACK packet")
	}
	return nil
}

func (b *mqttTelemetryBridge) nextSubscribePacketID() uint16 {
	b.nextPacketID++
	if b.nextPacketID == 0 {
		b.nextPacketID = 1
	}
	return b.nextPacketID
}

func mqttReadPacket(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	remaining, err := mqttReadRemainingLength(conn)
	if err != nil {
		return 0, nil, err
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return header[0], body, nil
}

func mqttReadRemainingLength(reader io.Reader) (int, error) {
	multiplier := 1
	value := 0
	for i := 0; i < 4; i++ {
		buf := make([]byte, 1)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return 0, err
		}
		value += int(buf[0]&0x7F) * multiplier
		if buf[0]&0x80 == 0 {
			return value, nil
		}
		multiplier *= 128
	}
	return 0, errors.New("MQTT remaining length exceeds 4 bytes")
}

func mqttParsePublish(header byte, packet []byte) (string, []byte, error) {
	if len(packet) < 2 {
		return "", nil, errors.New("short MQTT PUBLISH packet")
	}
	topicLength := int(binary.BigEndian.Uint16(packet[:2]))
	if len(packet) < 2+topicLength {
		return "", nil, errors.New("truncated MQTT topic in PUBLISH packet")
	}
	idx := 2 + topicLength
	qos := (header & 0x06) >> 1
	if qos > 0 {
		if len(packet) < idx+2 {
			return "", nil, errors.New("truncated MQTT packet identifier in PUBLISH packet")
		}
		idx += 2
	}
	return string(packet[2 : 2+topicLength]), packet[idx:], nil
}

func mqttPing(conn net.Conn) error {
	if _, err := conn.Write([]byte{0xC0, 0x00}); err != nil {
		return fmt.Errorf("write MQTT PINGREQ: %w", err)
	}
	return nil
}

func missingTopics(topics []string, subscribed map[string]struct{}) []string {
	missing := make([]string, 0)
	for _, topic := range topics {
		if _, ok := subscribed[topic]; ok {
			continue
		}
		missing = append(missing, topic)
	}
	return missing
}

func sortedTopicKeys(topics map[string]struct{}) []string {
	keys := make([]string, 0, len(topics))
	for topic := range topics {
		keys = append(keys, topic)
	}
	sort.Strings(keys)
	return keys
}

func waitForContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func shouldSendTelemetryPing(lastClientWrite, now time.Time) bool {
	return now.Sub(lastClientWrite) >= mqttTelemetryPingInterval
}
