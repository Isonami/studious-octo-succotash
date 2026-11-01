package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

const mqttNotificationTimeout = 5 * time.Second

type syncNotification struct {
	Event     string    `json:"event"`
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

type syncNotifyFunc func(event, id, path, state string, err error)

type mqttSyncNotifier struct {
	client mqtt.Client
	topic  string
	logger *slog.Logger
}

func validateMQTTConfig(config Config) error {
	if config.MQTTBroker == "" {
		return nil
	}
	u, err := url.Parse(config.MQTTBroker)
	if err != nil {
		return errors.New("invalid MQTT broker URL")
	}
	switch u.Scheme {
	case "tcp", "ssl", "tls", "ws", "wss":
	default:
		return errors.New("MQTT broker URL must use tcp, ssl, tls, ws or wss")
	}
	if u.Hostname() == "" {
		return errors.New("MQTT broker URL requires a host")
	}
	if config.MQTTTopic == "" || strings.ContainsAny(config.MQTTTopic, "+#\x00") {
		return errors.New("MQTT topic must be nonempty and cannot contain wildcards or NUL")
	}
	return nil
}

func newMQTTSyncNotifier(config Config, logger *slog.Logger) *mqttSyncNotifier {
	n := &mqttSyncNotifier{logger: logger, topic: config.MQTTTopic}
	if config.MQTTBroker == "" {
		return n
	}
	options := mqtt.NewClientOptions().AddBroker(config.MQTTBroker).
		SetClientID("sync-" + uuid.NewString()).SetCleanSession(true).
		SetAutoReconnect(false).SetConnectRetry(false).
		SetConnectTimeout(mqttNotificationTimeout).SetWriteTimeout(mqttNotificationTimeout).
		SetUsername(config.MQTTUsername).SetPassword(config.MQTTPassword)
	n.client = mqtt.NewClient(options)
	return n
}

func (n *mqttSyncNotifier) Notify(event, id, path, state string, syncErr error) {
	if n.client == nil {
		return
	}
	notification := syncNotification{Event: event, ID: id, Path: path, State: state, Timestamp: time.Now().UTC()}
	if syncErr != nil {
		notification.Error = syncErr.Error()
	}
	if err := n.publish(notification); err != nil {
		n.logger.Warn("MQTT sync notification failed", slog.String("event", event), slog.String("sync_id", id), slog.Any("error", err))
	}
}

func waitMQTTToken(token mqtt.Token) error {
	if !token.WaitTimeout(mqttNotificationTimeout) {
		return fmt.Errorf("MQTT operation timed out after %s", mqttNotificationTimeout)
	}
	return token.Error()
}

func (n *mqttSyncNotifier) publish(notification syncNotification) error {
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	if !n.client.IsConnected() {
		if err := waitMQTTToken(n.client.Connect()); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}
	// QoS 1 is acknowledged by the broker; consumers must tolerate duplicates.
	if err := waitMQTTToken(n.client.Publish(n.topic, 1, false, data)); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

func (n *mqttSyncNotifier) Close() {
	if n.client != nil {
		n.client.Disconnect(250)
	}
}
