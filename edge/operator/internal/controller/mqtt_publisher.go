package controller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type mqttPublishConfig struct {
	Broker   string
	Username string
	Password string
	ClientID string
}

func publishControlMessage(ctx context.Context, topic string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal MQTT command: %w", err)
	}

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
		cfg.ClientID = fmt.Sprintf("sif-operator-%d", time.Now().UnixNano())
	}

	return publishMQTT(ctx, cfg, topic, body)
}

func mqttBrokerAddress() string {
	broker := os.Getenv("SIF_MQTT_BROKER")
	if broker == "" {
		broker = "mqtt.caps-platform.de:1883"
	}
	broker = strings.TrimPrefix(broker, "mqtt://")
	broker = strings.TrimPrefix(broker, "tcp://")
	if !strings.Contains(broker, ":") {
		broker += ":1883"
	}
	return broker
}

func publishMQTT(ctx context.Context, cfg mqttPublishConfig, topic string, payload []byte) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Broker)
	if err != nil {
		return fmt.Errorf("connect MQTT broker %s: %w", cfg.Broker, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err := mqttConnect(conn, cfg); err != nil {
		return err
	}
	if err := mqttPublish(conn, topic, payload); err != nil {
		return err
	}
	_, _ = conn.Write([]byte{0xE0, 0x00})
	return nil
}

// mqttConnect and mqttPublish emit the minimal MQTT 3.1.1 packets needed by
// the operator, avoiding an extra runtime dependency in the controller image.
func mqttConnect(conn net.Conn, cfg mqttPublishConfig) error {
	variableHeader := append(mqttString("MQTT"), 0x04)
	flags := byte(0x02)
	if cfg.Username != "" {
		flags |= 0x80
	}
	if cfg.Password != "" {
		flags |= 0x40
	}
	variableHeader = append(variableHeader, flags, 0x00, 0x1E)

	payload := mqttString(cfg.ClientID)
	if cfg.Username != "" {
		payload = append(payload, mqttString(cfg.Username)...)
	}
	if cfg.Password != "" {
		payload = append(payload, mqttString(cfg.Password)...)
	}

	packet := mqttFixedHeader(0x10, len(variableHeader)+len(payload))
	packet = append(packet, variableHeader...)
	packet = append(packet, payload...)
	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("write MQTT CONNECT: %w", err)
	}

	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read MQTT CONNACK header: %w", err)
	}
	if header[0] != 0x20 {
		return fmt.Errorf("unexpected MQTT CONNACK packet type 0x%x", header[0])
	}
	body := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("read MQTT CONNACK body: %w", err)
	}
	if len(body) < 2 || body[1] != 0x00 {
		code := byte(0xFF)
		if len(body) >= 2 {
			code = body[1]
		}
		return fmt.Errorf("MQTT broker rejected connection, return code %d", code)
	}
	return nil
}

func mqttPublish(conn net.Conn, topic string, payload []byte) error {
	variableHeader := mqttString(topic)
	packet := mqttFixedHeader(0x30, len(variableHeader)+len(payload))
	packet = append(packet, variableHeader...)
	packet = append(packet, payload...)
	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("write MQTT PUBLISH: %w", err)
	}
	return nil
}

func mqttString(value string) []byte {
	buf := make([]byte, 2+len(value))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(value)))
	copy(buf[2:], value)
	return buf
}

func mqttFixedHeader(packetType byte, remainingLength int) []byte {
	header := []byte{packetType}
	for {
		encoded := byte(remainingLength % 128)
		remainingLength /= 128
		if remainingLength > 0 {
			encoded |= 128
		}
		header = append(header, encoded)
		if remainingLength == 0 {
			break
		}
	}
	return header
}
