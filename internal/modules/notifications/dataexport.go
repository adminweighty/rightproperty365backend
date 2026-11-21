package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// PathDataExport is the page where a person downloads their data export.
const PathDataExport = "/account/data-export"

// dataExportNotes tells the person their export is ready (API 0.55.0). The
// link opens the account page: the archive itself is only ever downloaded
// signed in.
func (h *Handlers) dataExportNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	var p struct {
		UserID    uuid.UUID `json:"user_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.UserID == uuid.Nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload", ev.Type))
	}
	people, err := h.source.Recipients(ctx, &p.UserID, nil)
	if err != nil {
		return err
	}
	for _, person := range people {
		d := mail.Data{Name: person.Name, Path: PathDataExport, ExpiresOn: p.ExpiresAt, When: occurredAt(ev)}
		if err := add("data-export-ready", "person", person, d); err != nil {
			return err
		}
	}
	return nil
}
