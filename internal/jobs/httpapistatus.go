package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HTTPAPIStatusPath is where the daemon records whether its HTTP API serves: <state>/http-api.status.
// status and system_doctor run in other processes and read it from there.
func HTTPAPIStatusPath(stateDir string) string { return filepath.Join(stateDir, "http-api.status") }

// HTTPAPIStatus is the content of http-api.status.
type HTTPAPIStatus struct {
	Serving bool `json:"serving"`
	// Listen is the bound address (the real port when the config asked for port 0) while serving.
	Listen string `json:"listen,omitempty"`
	// Reason is why the API does not serve (a bind failure, an unsafe state dir, the daemon stopped).
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// RecordHTTPAPI writes the API's state to http-api.status (mode 600, atomic). A failure to write is
// logged: the API itself does not depend on it.
func (m *Manager) RecordHTTPAPI(st HTTPAPIStatus) {
	st.At = m.Now().UTC()
	b, err := json.Marshal(st)
	if err == nil {
		err = writeAtomic(HTTPAPIStatusPath(m.StateDir), append(b, '\n'))
	}
	if err != nil {
		m.Log.Printf("http api: record status: %v", err)
	}
}

// HTTPAPICheck is the "http api" check of status and system_doctor; ok is false when http_api is off
// (then there is nothing to check).
func (m *Manager) HTTPAPICheck() (Check, bool) {
	if !m.Config().HTTPAPI.Enabled {
		return Check{}, false
	}
	c := Check{Name: "http api"}
	b, err := os.ReadFile(HTTPAPIStatusPath(m.StateDir))
	var st HTTPAPIStatus
	switch {
	case errors.Is(err, os.ErrNotExist):
		c.Detail = "http api not serving: no status recorded (the daemon has not started it)"
	case err != nil:
		c.Detail = "http api not serving: status unreadable: " + err.Error()
	case json.Unmarshal(b, &st) != nil:
		c.Detail = "http api not serving: status file is not valid JSON"
	case !st.Serving:
		c.Detail = fmt.Sprintf("http api not serving: %s (since %s)", st.Reason, st.At.Local().Format(time.DateTime))
	default:
		c.OK, c.Detail = true, fmt.Sprintf("listening on %s since %s", st.Listen, st.At.Local().Format(time.DateTime))
	}
	return c, true
}
