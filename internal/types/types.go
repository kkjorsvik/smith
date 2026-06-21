package types

import (
	"encoding/json"
	"time"
)

// Workload describes a container smith should keep running.
type Workload struct {
	ID          string       `json:"id"`
	Image       string       `json:"image"`
	Args        []string     `json:"args"`
	HealthCheck *HealthCheck `json:"health_check,omitempty"`
}

// HealthCheck defines how smith should probe a running container.
type HealthCheck struct {
	Type         string   `json:"type"`
	Command      []string `json:"command,omitempty"`
	URL          string   `json:"url,omitempty"`
	InitialDelay Duration `json:"initial_delay"`
	Interval     Duration `json:"interval"`
	Threshold    int      `json:"threshold"`
}

// Duration is a time.Duration that marshals to/from a human-readable
// string in JSON (e.g. "5s", "1m30s") instead of nanoseconds.
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}
