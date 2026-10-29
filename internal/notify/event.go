// Package notify sends operator incident metadata, never customer messages.
package notify

import (
	"fmt"
	"regexp"
	"time"
)

const MaxEventBytes = 4096

type Event struct {
	EventID     string    `json:"eventId"`
	IncidentID  string    `json:"incidentId"`
	NodeID      string    `json:"nodeId"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Occurrences int64     `json:"occurrences"`
	OccurredAt  time.Time `json:"occurredAt"`
}

var eventUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var metadataToken = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func (e Event) Valid() bool {
	return eventUUID.MatchString(e.IncidentID) && metadataToken.MatchString(e.NodeID) && metadataToken.MatchString(e.Kind) && (e.State == "open" || e.State == "resolved") && e.Occurrences > 0 && !e.OccurredAt.IsZero() && e.EventID == fmt.Sprintf("incident:%s:%d:%s", e.IncidentID, e.Occurrences, e.State)
}
