package kafkainject

import (
	"strings"
	"time"

	kgo "github.com/twmb/franz-go/pkg/kgo"
)

type kafkaProducerConfig struct {
	Acks     string `json:"acks"`
	LingerMs *int   `json:"lingerMs"`
}

type resolvedProducerSettings struct {
	AcksLabel    string
	LingerMs     int
	RequiredAcks kgo.Acks
}

func resolveProducerSettings(pc *kafkaProducerConfig) resolvedProducerSettings {
	settings := resolvedProducerSettings{
		AcksLabel:    DefaultProducerAcks,
		LingerMs:     DefaultProducerLingerMs,
		RequiredAcks: kgo.AllISRAcks(),
	}

	acksValue := DefaultProducerAcks
	if pc != nil && strings.TrimSpace(pc.Acks) != "" {
		acksValue = strings.TrimSpace(pc.Acks)
	}
	settings.AcksLabel, settings.RequiredAcks = mapAcks(acksValue)

	if pc != nil && pc.LingerMs != nil {
		settings.LingerMs = clampLingerMs(*pc.LingerMs)
	}
	return settings
}

func clampLingerMs(lingerMs int) int {
	if lingerMs < 0 {
		return 0
	}
	return lingerMs
}

func mapAcks(value string) (string, kgo.Acks) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "all", "-1":
		return "all", kgo.AllISRAcks()
	case "0", "none":
		return "0", kgo.NoAck()
	case "1", "leader":
		return "1", kgo.LeaderAck()
	default:
		return DefaultProducerAcks, kgo.AllISRAcks()
	}
}

func lingerDuration(lingerMs int) time.Duration {
	return time.Duration(lingerMs) * time.Millisecond
}
