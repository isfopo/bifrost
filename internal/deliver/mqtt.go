package deliver

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/url"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"bifrost/internal/config"
)

// deliverMQTT publishes the mapped payload (JSON) to an MQTT broker. The
// topic comes from the flow endpoint (JSONata-evaluable) or the service's
// fallback topic. Payload shape is entirely up to the flow's `map` —
// consumers see the object, or a string if the mapping produced one.
func (d *Dispatcher) deliverMQTT(o *config.ToService, endpoint string, mapped any) error {
	topic, err := mqttTopic(endpoint, o.Topic)
	if err != nil {
		return err
	}

	u, err := url.Parse(o.Broker)
	if err != nil {
		return fmt.Errorf("parse broker url %q: %w", o.Broker, err)
	}

	opts := mqtt.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(o.ClientID).
		SetAutoReconnect(false). // one-shot delivery; retries are the budget
		SetConnectTimeout(o.TimeoutDuration())

	switch o.Auth.Type {
	case "basic":
		user, pass := o.Auth.Basic()
		opts = opts.SetCredentialsProvider(func() (string, string) { return user, pass })
	}
	if u.Scheme == "ssl" || u.Scheme == "tls" || u.Scheme == "tcps" {
		opts = opts.SetTLSConfig(&tls.Config{
			ServerName:         u.Hostname(),
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: o.InsecureTLS,
		})
	}

	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.WaitTimeout(o.TimeoutDuration())
	if err = token.Error(); err != nil {
		return fmt.Errorf("mqtt connect %q: %w", o.Broker, err)
	}
	defer client.Disconnect(250) // let QoS > 0 in-flight finishes complete

	payload, err := json.Marshal(mapped)
	if err != nil {
		return fmt.Errorf("marshal mqtt payload: %w", err)
	}

	pub := client.Publish(topic, byte(o.QoS), o.Retain, payload)
	if !pub.WaitTimeout(o.TimeoutDuration()) {
		return fmt.Errorf("mqtt publish timed out after %s", o.TimeoutDuration())
	}
	if err := pub.Error(); err != nil {
		return fmt.Errorf("mqtt publish %q: %w", topic, err)
	}
	return nil
}

// mqttTopic resolves the publish topic: flow endpoint first, else the
// service fallback, else error. (The dispatcher already JSONata-resolves
// `$`-bearing endpoints before the switch.)
func mqttTopic(endpoint, fallback string) (string, error) {
	switch {
	case endpoint != "":
		return endpoint, nil
	case fallback != "":
		return fallback, nil
	default:
		return "", fmt.Errorf("mqtt target needs a topic: flow endpoint or service topic")
	}
}
