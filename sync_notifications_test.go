package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

type notificationToken struct {
	err      error
	timedOut bool
}

func (t notificationToken) Wait() bool                     { return !t.timedOut }
func (t notificationToken) WaitTimeout(time.Duration) bool { return !t.timedOut }
func (t notificationToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (t notificationToken) Error() error                   { return t.err }

type notificationClient struct {
	mqtt.Client
	connected              bool
	connects, disconnects  int
	connectErr, publishErr error
	notifications          []syncNotification
	topic                  string
	qos                    byte
	retained               bool
}

func (c *notificationClient) Connect() mqtt.Token {
	c.connects++
	c.connected = c.connectErr == nil
	return notificationToken{err: c.connectErr}
}
func (c *notificationClient) IsConnected() bool { return c.connected }
func (c *notificationClient) Disconnect(uint)   { c.disconnects++; c.connected = false }
func (c *notificationClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	c.topic = topic
	c.qos = qos
	c.retained = retained
	var n syncNotification
	if err := json.Unmarshal(payload.([]byte), &n); err != nil {
		return notificationToken{err: err}
	}
	c.notifications = append(c.notifications, n)
	return notificationToken{err: c.publishErr}
}
func notificationLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMQTTNotificationPayloadAndDelivery(t *testing.T) {
	client := &notificationClient{}
	n := &mqttSyncNotifier{client: client, topic: "app/sync", logger: notificationLogger()}
	n.Notify("start", "id", "/show", "running", nil)
	n.Notify("error", "id", "/show", "failed", errors.New("rsync failed"))
	n.Close()
	if client.connects != 1 || client.disconnects != 1 || client.topic != "app/sync" || client.qos != 1 || client.retained {
		t.Fatalf("unexpected client calls: %+v", client)
	}
	if len(client.notifications) != 2 {
		t.Fatal(client.notifications)
	}
	first, last := client.notifications[0], client.notifications[1]
	if first.Event != "start" || first.ID != "id" || first.Path != "/show" || first.State != "running" || first.Error != "" || first.Timestamp.IsZero() {
		t.Fatal(first)
	}
	if last.Event != "error" || last.Error != "rsync failed" {
		t.Fatal(last)
	}
}
func TestMQTTFailuresAreBestEffort(t *testing.T) {
	for _, client := range []*notificationClient{{connectErr: errors.New("offline")}, {publishErr: errors.New("disconnected")}} {
		n := &mqttSyncNotifier{client: client, topic: "app/sync", logger: notificationLogger()}
		n.Notify("complete", "id", "/show", "completed", nil)
		n.Close()
	}
	if err := waitMQTTToken(notificationToken{timedOut: true}); err == nil {
		t.Fatal("timeout ignored")
	}
	disabled := newMQTTSyncNotifier(Config{}, notificationLogger())
	if disabled.client != nil {
		t.Fatal("MQTT enabled without broker")
	}
	disabled.Notify("start", "id", "/show", "running", nil)
	disabled.Close()
}

// Exercise the real MQTT client against a minimal protocol-level broker,
// including authentication, PUBACK delivery, and JSON payload encoding.
func TestMQTTPublishOverTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	type received struct {
		username, password string
		notification       syncNotification
		err                error
	}
	results := make(chan received, 1)
	go func() {
		var result received
		defer func() { results <- result }()
		conn, err := listener.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		packet, err := packets.ReadPacket(conn)
		if err != nil {
			result.err = err
			return
		}
		connect, ok := packet.(*packets.ConnectPacket)
		if !ok {
			result.err = errors.New("expected CONNECT")
			return
		}
		result.username = connect.Username
		result.password = string(connect.Password)
		ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
		if err := ack.Write(conn); err != nil {
			result.err = err
			return
		}
		packet, err = packets.ReadPacket(conn)
		if err != nil {
			result.err = err
			return
		}
		publish, ok := packet.(*packets.PublishPacket)
		if !ok {
			result.err = errors.New("expected PUBLISH")
			return
		}
		if publish.TopicName != "sync/test" || publish.Qos != 1 || publish.Retain {
			result.err = errors.New("incorrect publish options")
			return
		}
		if err := json.Unmarshal(publish.Payload, &result.notification); err != nil {
			result.err = err
			return
		}
		puback := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
		puback.MessageID = publish.MessageID
		if err := puback.Write(conn); err != nil {
			result.err = err
			return
		}
		// Keep the connection alive until the client has consumed its PUBACK.
		_, result.err = packets.ReadPacket(conn)
	}()
	notifier := newMQTTSyncNotifier(Config{MQTTBroker: "tcp://" + listener.Addr().String(), MQTTTopic: "sync/test", MQTTUsername: "worker", MQTTPassword: "secret"}, notificationLogger())
	notifier.Notify("complete", "sync-id", "/show", "completed", nil)
	notifier.Close()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.username != "worker" || result.password != "secret" || result.notification.Event != "complete" || result.notification.ID != "sync-id" {
			t.Fatalf("incorrect MQTT message: %+v", result)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("MQTT broker did not receive notification")
	}
}

func TestMQTTConfiguration(t *testing.T) {
	for _, config := range []Config{
		{}, {MQTTBroker: "tcp://broker:1883", MQTTTopic: "sync/events"},
		{MQTTBroker: "ssl://broker:8883", MQTTTopic: "sync/events"},
		{MQTTBroker: "wss://broker/mqtt", MQTTTopic: "sync/events"},
	} {
		if err := validateMQTTConfig(config); err != nil {
			t.Fatal(config, err)
		}
	}
	for _, config := range []Config{
		{MQTTBroker: "http://broker", MQTTTopic: "sync/events"},
		{MQTTBroker: "tcp://", MQTTTopic: "sync/events"},
		{MQTTBroker: "tcp://broker", MQTTTopic: "sync/+"},
		{MQTTBroker: "tcp://broker", MQTTTopic: "sync/#"},
		{MQTTBroker: "tcp://broker", MQTTTopic: "sync/\x00"},
		{MQTTBroker: "tcp://broker"},
	} {
		if err := validateMQTTConfig(config); err == nil {
			t.Fatal("accepted invalid config", config)
		}
	}
}
func TestWorkerLifecycleNotifications(t *testing.T) {
	for _, test := range []struct {
		name, script string
		stop         bool
		events       []string
		wantErr      bool
	}{
		{name: "complete", script: "exit 0", events: []string{"start", "complete"}},
		{name: "error", script: "exit 23", events: []string{"start", "error"}, wantErr: true},
		{name: "stopped before start", stop: true, events: []string{"cancelled"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte("#!/bin/sh\n"+test.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			id, _, err := store.reserve(ctx, "/show", "queued")
			if err != nil {
				t.Fatal(err)
			}
			if test.stop {
				if err := store.stop(ctx, "/show"); err != nil {
					t.Fatal(err)
				}
			}
			var events []string
			notify := func(event, gotID, path, state string, eventErr error) {
				events = append(events, event)
				if gotID != id || path != "/show" {
					t.Errorf("incorrect sync identity: %s %s", gotID, path)
				}
				if (event == "error") != (eventErr != nil) {
					t.Errorf("event=%s error=%v", event, eventErr)
				}
			}
			err = runSyncWorkerWithNotifications(ctx, notificationLogger(), Config{SyncWorker: id, DataPath: t.TempDir()}, store, notify)
			if (err != nil) != test.wantErr {
				t.Fatalf("worker error=%v", err)
			}
			if !reflect.DeepEqual(events, test.events) {
				t.Fatalf("events=%v want %v", events, test.events)
			}
		})
	}
}
