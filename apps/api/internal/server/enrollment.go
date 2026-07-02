package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ufo/apps/api/internal/db"
)

const roverStreamKeepalive = 20 * time.Second

func (s *Server) authRoverOrUser(w http.ResponseWriter, r *http.Request) (*db.Rover, *http.Request, bool) {
	if token := bearerToken(r); token != "" && !strings.Contains(token, ".") {
		rv, ok := s.authenticateRover(w, r, token)
		if !ok {
			return nil, r, false
		}
		return &rv, r, true
	}
	user, ok := s.resolveUserForRequest(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "not authenticated")
		return nil, r, false
	}
	return nil, r.WithContext(context.WithValue(r.Context(), userKey, user)), true
}

func (s *Server) resolveUserForRequest(r *http.Request) (db.User, bool) {
	if token := bearerToken(r); token != "" {
		if user, err := s.userFromAccessToken(r.Context(), token); err == nil {
			return user, true
		}
		return db.User{}, false
	}
	// No response writer here — skip clearing a bad access cookie.
	return s.userFromCookies(nil, r, false)
}

func (s *Server) getRover(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	roverActor, r, ok := s.authRoverOrUser(w, r)
	if !ok {
		return
	}
	rover, err := s.q.GetRoverByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "rover not found")
		return
	}
	if roverActor != nil {
		if roverActor.ID != rover.ID {
			httpError(w, http.StatusForbidden, "not allowed to read this rover")
			return
		}
	} else if s.memberRole(r, rover.FleetID) == "" {
		httpError(w, http.StatusForbidden, "not a member of this fleet")
		return
	}
	fleet, err := s.q.GetFleetByID(r.Context(), rover.FleetID)
	if err != nil {
		serverError(w, err)
		return
	}
	d := roverDTOFromRover(rover, 0)
	d.FleetID = uuidStr(fleet.PublicID)
	d.FleetName = fleet.Name
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) roverStream(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	rv := currentRover(r)
	self, err := s.q.GetRoverByPublicID(r.Context(), pid)
	if err != nil || self.ID != rv.ID {
		httpError(w, http.StatusForbidden, "not allowed to stream this rover")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ctx := r.Context()
	sub, unsub := s.notifier.Subscribe(changedChannel)
	defer unsub()
	writeRoverConfigEvent(w, s.roverConfigEvent(ctx, self))
	flusher.Flush()
	ticker := time.NewTicker(roverStreamKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ":\n\n")
			flusher.Flush()
		case note := <-sub:
			var payload struct {
				T     string `json:"t"`
				Fleet int64  `json:"fleet"`
			}
			if json.Unmarshal([]byte(note.Payload), &payload) != nil {
				continue
			}
			if payload.Fleet != rv.FleetID || (payload.T != "rover" && payload.T != "fleet") {
				continue
			}
			fresh, err := s.q.GetRoverByPublicID(ctx, pid)
			if err != nil {
				fmt.Fprint(w, "event: revoke\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			writeRoverConfigEvent(w, s.roverConfigEvent(ctx, fresh))
			flusher.Flush()
		}
	}
}

type roverConfigEvent struct {
	Name      string   `json:"name"`
	Units     int32    `json:"units"`
	Tags      []string `json:"tags"`
	FleetID   string   `json:"fleet_id,omitempty"`
	FleetName string   `json:"fleet_name,omitempty"`
}

func (s *Server) roverConfigEvent(ctx context.Context, rover db.Rover) roverConfigEvent {
	event := roverConfigEvent{Name: rover.Name, Units: rover.Units, Tags: rover.Tags}
	if fleet, err := s.q.GetFleetByID(ctx, rover.FleetID); err == nil {
		event.FleetID = uuidStr(fleet.PublicID)
		event.FleetName = fleet.Name
	}
	return event
}

func writeRoverConfigEvent(w http.ResponseWriter, event roverConfigEvent) {
	payload, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: config\ndata: %s\n\n", payload)
}
