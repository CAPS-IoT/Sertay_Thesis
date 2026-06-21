package controller

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
)

func TestMQTTPublishUsesQoSOneAndWaitsForMatchingPUBACK(t *testing.T) {
	client, broker := net.Pipe()
	defer client.Close()

	brokerDone := make(chan error, 1)
	go func() {
		defer broker.Close()
		header := make([]byte, 2)
		if _, err := io.ReadFull(broker, header); err != nil {
			brokerDone <- err
			return
		}
		if header[0] != 0x32 {
			brokerDone <- fmt.Errorf("PUBLISH header = %#x, want %#x", header[0], byte(0x32))
			return
		}
		body := make([]byte, int(header[1]))
		if _, err := io.ReadFull(broker, body); err != nil {
			brokerDone <- err
			return
		}
		topicLength := int(binary.BigEndian.Uint16(body[:2]))
		packetIDOffset := 2 + topicLength
		if packetIDOffset+2 > len(body) ||
			binary.BigEndian.Uint16(body[packetIDOffset:packetIDOffset+2]) != 1 {
			brokerDone <- fmt.Errorf("PUBLISH packet identifier is not 1")
			return
		}
		_, err := broker.Write([]byte{0x40, 0x02, 0x00, 0x01})
		brokerDone <- err
	}()

	if err := mqttPublish(client, "device/control", []byte(`{"action":"stage_release"}`)); err != nil {
		t.Fatalf("mqttPublish() error = %v", err)
	}
	if err := <-brokerDone; err != nil {
		t.Fatalf("broker side error = %v", err)
	}
}

func TestMQTTPublishRejectsMismatchedPUBACK(t *testing.T) {
	client, broker := net.Pipe()
	defer client.Close()

	go func() {
		defer broker.Close()
		header := make([]byte, 2)
		_, _ = io.ReadFull(broker, header)
		body := make([]byte, int(header[1]))
		_, _ = io.ReadFull(broker, body)
		_, _ = broker.Write([]byte{0x40, 0x02, 0x00, 0x02})
	}()

	if err := mqttPublish(client, "device/control", []byte("{}")); err == nil {
		t.Fatal("mqttPublish() accepted a PUBACK for the wrong packet ID")
	}
}
