package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// OrchestrationHandler serves read-only orchestration mode info.
type OrchestrationHandler struct {
	agents store.AgentStore
	teams  store.TeamStore
	links  store.AgentLinkStore
}

func NewOrchestrationHandler(agents store.AgentStore, teams store.TeamStore, links store.AgentLinkStore) *OrchestrationHandler {
	return &OrchestrationHandler{agents: agents, teams: teams, links: links}
}

func (h *OrchestrationHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents/{agentID}/orchestration", h.auth(h.handleGetMode))
	mux.HandleFunc("POST /v1/agents/{agentID}/links", h.adminAuth(h.handleCreateLink))
	mux.HandleFunc("PUT /v1/agents/{agentID}/context-files/{name}", h.adminAuth(h.handleSetContextFile))
}

// seedableContextFiles is the set of identity context files an admin seed may
// overwrite directly. Operational templates (AGENTS.md, TOOLS.md) stay fixed and
// are intentionally excluded.
var seedableContextFiles = map[string]bool{
	bootstrap.SoulFile:         true,
	bootstrap.IdentityFile:     true,
	bootstrap.CapabilitiesFile: true,
}

func (h *OrchestrationHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

func (h *OrchestrationHandler) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(permissions.RoleAdmin, next)
}

// handleGetMode returns the computed orchestration mode and delegate targets for an agent.
func (h *OrchestrationHandler) handleGetMode(w http.ResponseWriter, r *http.Request) {
	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}

	ctx := r.Context()
	mode := agent.ResolveOrchestrationMode(ctx, agentID, h.teams, h.links)

	resp := map[string]any{
		"mode":             string(mode),
		"delegate_targets": []any{},
		"team":             nil,
	}

	// Populate delegate targets if in delegate or team mode.
	if h.links != nil {
		targets, err := h.links.DelegateTargets(ctx, agentID)
		if err != nil {
			slog.Warn("orchestration.delegate_targets failed", "error", err)
		} else if len(targets) > 0 {
			entries := make([]map[string]string, 0, len(targets))
			for _, t := range targets {
				entries = append(entries, map[string]string{
					"agent_key":    t.TargetAgentKey,
					"display_name": t.TargetDisplayName,
				})
			}
			resp["delegate_targets"] = entries
		}
	}

	// Populate team info if in team mode.
	if mode == agent.ModeTeam && h.teams != nil {
		if team, err := h.teams.GetTeamForAgent(ctx, agentID); err == nil && team != nil {
			resp["team"] = map[string]any{
				"id":   team.ID,
				"name": team.Name,
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

type createLinkBody struct {
	TargetAgent   string `json:"target_agent"`
	Direction     string `json:"direction"`
	Description   string `json:"description"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// handleCreateLink creates an outbound delegation link from the path agent to a
// target agent so the source can delegate to it. Idempotent: returns the
// existing link when one already connects the pair.
func (h *OrchestrationHandler) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	if h.links == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "links not configured"})
		return
	}

	ctx := r.Context()

	sourceID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}
	source, err := h.agents.GetByID(ctx, sourceID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "source agent not found"})
		return
	}

	var body createLinkBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.TargetAgent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_agent is required"})
		return
	}

	target, err := h.resolveAgent(ctx, body.TargetAgent)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target agent not found"})
		return
	}
	if source.ID == target.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source and target must differ"})
		return
	}
	if target.AgentType == store.AgentTypeOpen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delegate to an open agent"})
		return
	}

	direction := body.Direction
	if direction == "" {
		direction = store.LinkDirectionOutbound
	}
	if direction != store.LinkDirectionOutbound && direction != store.LinkDirectionInbound && direction != store.LinkDirectionBidirectional {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid direction"})
		return
	}

	if existing, _ := h.links.GetLinkBetween(ctx, source.ID, target.ID); existing != nil {
		writeJSON(w, http.StatusOK, map[string]any{"link": existing, "created": false})
		return
	}

	maxConcurrent := body.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}

	link := &store.AgentLinkData{
		SourceAgentID: source.ID,
		TargetAgentID: target.ID,
		Direction:     direction,
		Description:   body.Description,
		MaxConcurrent: maxConcurrent,
		Status:        store.LinkStatusActive,
		CreatedBy:     store.UserIDFromContext(ctx),
	}
	if err := h.links.CreateLink(ctx, link); err != nil {
		slog.Warn("orchestration.create_link failed", "source", source.AgentKey, "target", target.AgentKey, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create link"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"link": link, "created": true})
}

type setContextFileBody struct {
	Content string `json:"content"`
}

// handleSetContextFile overwrites a single identity context file (SOUL.md,
// IDENTITY.md, CAPABILITIES.md) for an agent. Lets the ai-claw orchestrator seed
// give predefined agents distinct, reviewable identities deterministically,
// without depending on the LLM summon step or a configured provider.
func (h *OrchestrationHandler) handleSetContextFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent ID"})
		return
	}

	name := r.PathValue("name")
	if !seedableContextFiles[name] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "context file not writable"})
		return
	}

	var body setContextFileBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}

	if _, err := h.agents.GetByID(ctx, agentID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}

	if err := h.agents.SetAgentContextFile(ctx, agentID, name, body.Content); err != nil {
		slog.Warn("orchestration.set_context_file failed", "agent", agentID, "file", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to set context file"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"agent_id": agentID, "file": name, "updated": true})
}

func (h *OrchestrationHandler) resolveAgent(ctx context.Context, keyOrID string) (*store.AgentData, error) {
	if id, err := uuid.Parse(keyOrID); err == nil {
		return h.agents.GetByID(ctx, id)
	}
	return h.agents.GetByKey(ctx, keyOrID)
}
