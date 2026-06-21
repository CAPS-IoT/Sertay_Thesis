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
	"sync"
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
	client            client.Client
	nextPacketID      uint16
	deadlineEstimator *DeadlineTelemetryEstimator
	drainTracker      *batteryDrainTracker
}

type batteryTelemetry struct {
	BatteryPercent       *int32  `json:"batteryPercent"`
	Mode                 string  `json:"mode,omitempty"`
	Source               string  `json:"source,omitempty"`
	VoltageMV            *int32  `json:"voltageMv,omitempty"`
	ArtifactDigest       string  `json:"artifactDigest,omitempty"`
	ReleaseGeneration    *int64  `json:"releaseGeneration,omitempty"`
	StagedArtifactDigest *string `json:"stagedArtifactDigest,omitempty"`
	StagedGeneration     *int64  `json:"stagedReleaseGeneration,omitempty"`
	AdmissionPaused      *bool   `json:"admissionPaused,omitempty"`
	Function             string  `json:"function,omitempty"`
	ExecutionMs          *int32  `json:"executionMs,omitempty"`
	QueueDelayMs         *int32  `json:"queueDelayMs,omitempty"`
	ResourceWakeMs       *int32  `json:"resourceWakeMs,omitempty"`
	ResourceCollectionMs *int32  `json:"resourceCollectionMs,omitempty"`
	NetworkRoundTripMs   *int32  `json:"networkRoundTripMs,omitempty"`
	EdgeExecutionMs      *int32  `json:"edgeExecutionMs,omitempty"`
	OutputApplicationMs  *int32  `json:"outputApplicationMs,omitempty"`
}

type telemetryUpdateResult struct {
	statusChanged                bool
	reportableInfo               bool
	stagedReleaseAcknowledged    bool
	activatedReleaseAcknowledged bool
}

type batteryDrainSample struct {
	at      time.Time
	battery int32
}

type batteryDrainState struct {
	source       string
	function     string
	mode         string
	lastObserved time.Time
	samples      []batteryDrainSample
	riskyWindows int32
}

type batteryDrainTracker struct {
	mu     sync.Mutex
	states map[types.NamespacedName]*batteryDrainState
}

func newBatteryDrainTracker() *batteryDrainTracker {
	return &batteryDrainTracker{states: map[types.NamespacedName]*batteryDrainState{}}
}

func NewMQTTTelemetryBridge(kubeClient client.Client, estimators ...*DeadlineTelemetryEstimator) *mqttTelemetryBridge {
	estimator := newDeadlineTelemetryEstimator()
	if len(estimators) > 0 && estimators[0] != nil {
		estimator = estimators[0]
	}
	return &mqttTelemetryBridge{
		client: kubeClient, deadlineEstimator: estimator,
		drainTracker: newBatteryDrainTracker(),
	}
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
	telemetry.Function = strings.TrimSpace(telemetry.Function)
	telemetry.ArtifactDigest = normalizeArtifactDigest(telemetry.ArtifactDigest)
	if telemetry.StagedArtifactDigest != nil {
		normalized := normalizeArtifactDigest(*telemetry.StagedArtifactDigest)
		telemetry.StagedArtifactDigest = &normalized
	}
	if telemetry.VoltageMV != nil && *telemetry.VoltageMV < 0 {
		zero := int32(0)
		telemetry.VoltageMV = &zero
	}
	if telemetry.ExecutionMs != nil && *telemetry.ExecutionMs < 0 {
		zero := int32(0)
		telemetry.ExecutionMs = &zero
	}
	clampNonNegativeInt32(&telemetry.QueueDelayMs)
	clampNonNegativeInt32(&telemetry.ResourceWakeMs)
	clampNonNegativeInt32(&telemetry.ResourceCollectionMs)
	clampNonNegativeInt32(&telemetry.NetworkRoundTripMs)
	clampNonNegativeInt32(&telemetry.EdgeExecutionMs)
	clampNonNegativeInt32(&telemetry.OutputApplicationMs)

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
		if result.stagedReleaseAcknowledged {
			log.Info("Device acknowledged staged release",
				"namespace", key.Namespace,
				"name", key.Name,
				"releaseGeneration", wf.Spec.Release.Generation,
				"functionIdentity", wf.Spec.Release.FunctionIdentity,
				"artifactDigest", artifactDigestForLog(wf.Spec.Release.ArtifactDigest))
		}
		if result.activatedReleaseAcknowledged {
			log.Info("Device acknowledged active release",
				"namespace", key.Namespace,
				"name", key.Name,
				"releaseGeneration", wf.Spec.Release.Generation,
				"functionIdentity", wf.Spec.Release.FunctionIdentity,
				"artifactDigest", artifactDigestForLog(wf.Spec.Release.ArtifactDigest))
		}
		if result.reportableInfo {
			keysAndValues := []interface{}{
				"namespace", key.Namespace,
				"name", key.Name,
				"topic", topic,
				"batteryPercent", battery,
				"mode", telemetry.Mode,
				"batterySource", telemetry.Source,
				"observedArtifactDigest", artifactDigestForLog(telemetry.ArtifactDigest),
			}
			if telemetry.Function != "" {
				keysAndValues = append(keysAndValues, "telemetryKind", "invocation", "function", telemetry.Function)
			} else {
				keysAndValues = append(keysAndValues, "telemetryKind", "state")
			}
			log.Info("Applied MQTT telemetry", keysAndValues...)
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
		now := metav1.Now()
		desiredDigest := normalizeArtifactDigest(wf.Spec.Release.ArtifactDigest)
		wasStaged := deviceReleaseStaged(&wf, desiredDigest)
		wasActive := deviceReleaseActive(&wf, desiredDigest)
		b.updateBatteryDeltaStatus(key, &wf, telemetry, battery, now.Time, &result)
		b.deadlineEstimator.recordAndEstimate(&wf, telemetry, now.Time)

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
		if telemetry.AdmissionPaused != nil && wf.Status.ObservedAdmissionPaused != *telemetry.AdmissionPaused {
			wf.Status.ObservedAdmissionPaused = *telemetry.AdmissionPaused
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
		if telemetry.ReleaseGeneration != nil && wf.Status.AcknowledgedReleaseGeneration != *telemetry.ReleaseGeneration {
			wf.Status.AcknowledgedReleaseGeneration = *telemetry.ReleaseGeneration
			result.statusChanged = true
			result.reportableInfo = true
		}
		if telemetry.StagedArtifactDigest != nil && wf.Status.DeviceStagedArtifactDigest != *telemetry.StagedArtifactDigest {
			wf.Status.DeviceStagedArtifactDigest = *telemetry.StagedArtifactDigest
			result.statusChanged = true
			result.reportableInfo = true
		}
		if telemetry.StagedGeneration != nil && wf.Status.StagedReleaseGeneration != *telemetry.StagedGeneration {
			wf.Status.StagedReleaseGeneration = *telemetry.StagedGeneration
			result.statusChanged = true
			result.reportableInfo = true
		}
		if telemetry.Function != "" && wf.Status.ObservedFunction != telemetry.Function {
			wf.Status.ObservedFunction = telemetry.Function
			result.statusChanged = true
			result.reportableInfo = true
		}
		result.stagedReleaseAcknowledged =
			!wasStaged && deviceReleaseStaged(&wf, desiredDigest)
		result.activatedReleaseAcknowledged =
			!wasActive && deviceReleaseActive(&wf, desiredDigest)
		wf.Status.LastTelemetryTime = &now
		return b.client.Status().Patch(ctx, &wf, client.MergeFrom(base))
	})
	return result, err
}

func (b *mqttTelemetryBridge) updateBatteryDeltaStatus(key types.NamespacedName, wf *edgev1alpha1.WasmFunction, telemetry batteryTelemetry, battery int32, now time.Time, result *telemetryUpdateResult) {
	if !effectiveBatteryDeltaEnabled(wf) {
		b.drainTracker.mu.Lock()
		delete(b.drainTracker.states, key)
		b.drainTracker.mu.Unlock()
		if wf.Status.ObservedBatteryDeltaPercent != nil || wf.Status.ObservedBatteryDeltaWindowSeconds != nil || wf.Status.ConsecutiveRiskyWindows != 0 {
			wf.Status.ObservedBatteryDeltaPercent = nil
			wf.Status.ObservedBatteryDeltaWindowSeconds = nil
			wf.Status.ConsecutiveRiskyWindows = 0
			result.statusChanged = true
		}
		return
	}

	window := effectiveBatteryDeltaWindowSeconds(wf)
	mode := latestTelemetryMode(wf, telemetry)
	source := telemetry.Source
	if source == "" {
		source = wf.Status.ObservedBatterySource
	}
	function := telemetry.Function
	if function == "" {
		function = strings.TrimSpace(wf.Spec.Release.FunctionIdentity)
	}

	b.drainTracker.mu.Lock()
	defer b.drainTracker.mu.Unlock()
	state := b.drainTracker.states[key]
	gap := state != nil && !state.lastObserved.IsZero() && now.Sub(state.lastObserved) > time.Duration(window)*time.Second
	if state == nil || state.source != source || state.function != function || state.mode != mode || gap {
		state = &batteryDrainState{source: source, function: function, mode: mode}
		b.drainTracker.states[key] = state
	}
	state.lastObserved = now

	low := wf.Spec.Placement.LowBatteryThreshold
	if low == 0 {
		low = defaultLowBatteryThreshold
	}
	high := wf.Spec.Placement.HighBatteryThreshold
	if high == 0 {
		high = defaultHighBatteryThreshold
	}
	applicable := mode == placementLocal && battery > low && battery < high
	if !applicable {
		state.samples = nil
		state.riskyWindows = 0
		setBatteryDrainStatus(wf, window, nil, 0, result)
		return
	}

	cutoff := now.Add(-time.Duration(window) * time.Second)
	kept := state.samples[:0]
	for _, sample := range state.samples {
		if !sample.at.Before(cutoff) {
			kept = append(kept, sample)
		}
	}
	state.samples = append(kept, batteryDrainSample{at: now, battery: battery})
	if len(state.samples) < 2 {
		state.riskyWindows = 0
		setBatteryDrainStatus(wf, window, nil, 0, result)
		return
	}

	delta := state.samples[0].battery - battery
	if delta < 0 {
		delta = 0
	}
	if delta >= effectiveBatteryDeltaMaxDrainPercent(wf) {
		state.riskyWindows++
	} else {
		state.riskyWindows = 0
	}
	setBatteryDrainStatus(wf, window, &delta, state.riskyWindows, result)
}

func setBatteryDrainStatus(wf *edgev1alpha1.WasmFunction, window int32, delta *int32, riskyWindows int32, result *telemetryUpdateResult) {
	if !equalInt32Ptr(wf.Status.ObservedBatteryDeltaPercent, delta) {
		wf.Status.ObservedBatteryDeltaPercent = copyInt32Ptr(delta)
		result.statusChanged = true
	}
	if wf.Status.ObservedBatteryDeltaWindowSeconds == nil || *wf.Status.ObservedBatteryDeltaWindowSeconds != window {
		wf.Status.ObservedBatteryDeltaWindowSeconds = int32Ptr(window)
		result.statusChanged = true
	}
	if wf.Status.ConsecutiveRiskyWindows != riskyWindows {
		wf.Status.ConsecutiveRiskyWindows = riskyWindows
		result.statusChanged = true
	}
}

func equalInt32Ptr(left, right *int32) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func latestTelemetryMode(wf *edgev1alpha1.WasmFunction, telemetry batteryTelemetry) string {
	if telemetry.Mode != "" {
		return telemetry.Mode
	}
	return wf.Status.ObservedMode
}

func updateInt32Ptr(dst **int32, src *int32) bool {
	if src == nil {
		return false
	}
	value := *src
	if *dst != nil && **dst == value {
		return false
	}
	*dst = &value
	return true
}

func clampNonNegativeInt32(value **int32) {
	if value == nil || *value == nil || **value >= 0 {
		return
	}
	zero := int32(0)
	*value = &zero
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
