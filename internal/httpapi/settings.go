package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// providerDTO is a registry entry as the API exposes it.
//
// Note what is absent: the key. The registry stores the env var NAME, and this
// carries that name plus whether it currently resolves. A key value must never
// leave the process, and "is it set" is the only thing the settings screen
// actually needs to render its badge.
type providerDTO struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Keyless   bool   `json:"keyless"`
	Resolved  bool   `json:"resolved"`
}

type settingsResponse struct {
	Providers []providerDTO `json:"providers"`

	ClassifierModel     string   `json:"classifier_model"`
	SynthesizerModel    string   `json:"synthesizer_model"`
	ClassifierFallback  []string `json:"classifier_fallback"`
	SynthesizerFallback []string `json:"synthesizer_fallback"`

	ScheduleIngest     string `json:"schedule_ingest"`
	ScheduleSynthesize string `json:"schedule_synthesize"`
	Timezone           string `json:"timezone"`

	MaxCallsPerRun int `json:"max_calls_per_run"`
	MaxCallsPerDay int `json:"max_calls_per_day"`

	HNMinPoints       int     `json:"hn_min_points"`
	SkyDays           int     `json:"sky_days"`
	ClassifyBatchSize int     `json:"classify_batch_size"`
	HotThreshold      int     `json:"hot_threshold"`
	MatchThreshold    float64 `json:"match_threshold"`
}

// settingsRequest is a partial update: every field is optional, so the UI can
// save one screen without echoing back the others.
type settingsRequest struct {
	Providers *[]struct {
		Name      string `json:"name"`
		BaseURL   string `json:"base_url"`
		APIKeyEnv string `json:"api_key_env"`
	} `json:"providers"`

	ClassifierModel     *string   `json:"classifier_model"`
	SynthesizerModel    *string   `json:"synthesizer_model"`
	ClassifierFallback  *[]string `json:"classifier_fallback"`
	SynthesizerFallback *[]string `json:"synthesizer_fallback"`

	ScheduleIngest     *string `json:"schedule_ingest"`
	ScheduleSynthesize *string `json:"schedule_synthesize"`

	MaxCallsPerRun *int `json:"max_calls_per_run"`
	MaxCallsPerDay *int `json:"max_calls_per_day"`

	HNMinPoints       *int     `json:"hn_min_points"`
	SkyDays           *int     `json:"sky_days"`
	ClassifyBatchSize *int     `json:"classify_batch_size"`
	HotThreshold      *int     `json:"hot_threshold"`
	MatchThreshold    *float64 `json:"match_threshold"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}

	out := settingsResponse{
		ClassifierModel:     settings[store.KeyClassifierModel],
		SynthesizerModel:    settings[store.KeySynthesizerModel],
		ClassifierFallback:  stringList(settings[store.KeyClassifierFallback]),
		SynthesizerFallback: stringList(settings[store.KeySynthFallback]),
		ScheduleIngest:      settings[store.KeyScheduleIngest],
		ScheduleSynthesize:  settings[store.KeyScheduleSynthesis],
		Timezone:            s.Env.Location.String(),
		MaxCallsPerRun:      intOf(settings[store.KeyMaxCallsPerRun], 200),
		MaxCallsPerDay:      intOf(settings[store.KeyMaxCallsPerDay], 500),
		HNMinPoints:         intOf(settings[store.KeyHNMinPoints], 25),
		SkyDays:             intOf(settings[store.KeySkyDays], 7),
		ClassifyBatchSize:   intOf(settings[store.KeyClassifyBatchSize], 30),
		HotThreshold:        intOf(settings[store.KeyHotThreshold], 3),
		MatchThreshold:      floatOf(settings[store.KeyMatchThreshold], 4),
	}

	// A malformed registry still has to render, or the owner cannot fix it in the
	// very screen meant for fixing it.
	if registry, err := llm.ParseRegistry(settings[store.KeyProviders]); err == nil {
		for _, p := range registry.Providers() {
			_, resolved := p.APIKey()
			out.Providers = append(out.Providers, providerDTO{
				Name: p.Name, BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv,
				Keyless: p.Keyless(), Resolved: resolved,
			})
		}
	}
	if out.Providers == nil {
		out.Providers = []providerDTO{}
	}

	s.json(w, http.StatusOK, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}

	updates := map[string]string{}

	if req.Providers != nil {
		// Marshal from the DTO, not from the request body: that drops any field
		// a client tried to smuggle in, so nothing resembling a key can be
		// persisted even by accident.
		clean := make([]llm.Provider, 0, len(*req.Providers))
		for _, p := range *req.Providers {
			clean = append(clean, llm.Provider{
				Name:      strings.TrimSpace(p.Name),
				BaseURL:   strings.TrimSpace(p.BaseURL),
				APIKeyEnv: strings.TrimSpace(p.APIKeyEnv),
			})
		}
		raw, err := json.Marshal(clean)
		if err != nil {
			s.error(w, http.StatusInternalServerError, "could not encode providers", err)
			return
		}
		updates[store.KeyProviders] = string(raw)
	}

	setStr := func(key string, v *string) {
		if v != nil {
			updates[key] = strings.TrimSpace(*v)
		}
	}
	setInt := func(key string, v *int) {
		if v != nil {
			updates[key] = strconv.Itoa(*v)
		}
	}

	setStr(store.KeyClassifierModel, req.ClassifierModel)
	setStr(store.KeySynthesizerModel, req.SynthesizerModel)
	setStr(store.KeyScheduleIngest, req.ScheduleIngest)
	setStr(store.KeyScheduleSynthesis, req.ScheduleSynthesize)

	if req.ClassifierFallback != nil {
		updates[store.KeyClassifierFallback] = mustJSON(*req.ClassifierFallback)
	}
	if req.SynthesizerFallback != nil {
		updates[store.KeySynthFallback] = mustJSON(*req.SynthesizerFallback)
	}

	setInt(store.KeyMaxCallsPerRun, req.MaxCallsPerRun)
	setInt(store.KeyMaxCallsPerDay, req.MaxCallsPerDay)
	setInt(store.KeyHNMinPoints, req.HNMinPoints)
	setInt(store.KeySkyDays, req.SkyDays)
	setInt(store.KeyClassifyBatchSize, req.ClassifyBatchSize)
	setInt(store.KeyHotThreshold, req.HotThreshold)
	if req.MatchThreshold != nil {
		updates[store.KeyMatchThreshold] = strconv.FormatFloat(*req.MatchThreshold, 'f', -1, 64)
	}

	if len(updates) == 0 {
		s.error(w, http.StatusBadRequest, "nothing to update", nil)
		return
	}

	// Validate the result before persisting. Saving a broken registry would take
	// the next run down, and the owner would have to guess which edit did it.
	if err := s.validateSettingsUpdate(updates); err != nil {
		s.error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	if err := s.DB.SetSettings(updates); err != nil {
		s.error(w, http.StatusInternalServerError, "could not save settings", err)
		return
	}

	// The schedule lives in cron entries, so a time change has to be applied to
	// the running scheduler. This is what makes "no restart" true for §7's
	// schedule as well as for model routing.
	if _, ok := updates[store.KeyScheduleIngest]; ok {
		s.reloadSchedule()
	} else if _, ok := updates[store.KeyScheduleSynthesis]; ok {
		s.reloadSchedule()
	}

	s.handleGetSettings(w, r)
}

// validateSettingsUpdate applies the pending change on top of current settings
// and checks the result parses.
func (s *Server) validateSettingsUpdate(updates map[string]string) error {
	current, err := s.DB.Settings()
	if err != nil {
		return fmt.Errorf("could not read current settings")
	}
	merged := make(map[string]string, len(current)+len(updates))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}

	if _, err := llm.LoadConfig(merged); err != nil {
		return err
	}
	for _, key := range []string{store.KeyScheduleIngest, store.KeyScheduleSynthesis} {
		if v, ok := updates[key]; ok {
			if err := validateHHMM(v); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

func (s *Server) reloadSchedule() {
	if s.Scheduler == nil {
		return
	}
	if err := s.Scheduler.Reload(); err != nil {
		// The scheduler keeps its previous entries on failure, so this is a
		// warning rather than a rollback.
		s.Log.Warn("could not reload schedule", "err", err)
	}
}

func validateHHMM(v string) error {
	hour, minute, found := strings.Cut(strings.TrimSpace(v), ":")
	if !found {
		return fmt.Errorf("want HH:MM")
	}
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return fmt.Errorf("hour must be 0-23")
	}
	m, err := strconv.Atoi(minute)
	if err != nil || m < 0 || m > 59 {
		return fmt.Errorf("minute must be 0-59")
	}
	return nil
}

// testRequest asks for a canned call through one role's resolved provider.
type testRequest struct {
	Role string `json:"role"`
}

type testResponse struct {
	OK        bool   `json:"ok"`
	Role      string `json:"role"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Reply     string `json:"reply,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	Error     string `json:"error,omitempty"`
}

// handleModelTest is §3.1's "test call" button and the second half of §10's
// no-restart criterion: after reassigning a role, this proves the new provider
// answers before waiting for a run.
//
// It reads settings fresh, like every other path, which is exactly why no
// restart is needed.
func (s *Server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		s.error(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}

	role := llm.Role(strings.TrimSpace(req.Role))
	if role != llm.RoleClassifier && role != llm.RoleSynthesizer {
		s.error(w, http.StatusBadRequest, "role must be classifier or synthesizer", nil)
		return
	}

	settings, err := s.DB.Settings()
	if err != nil {
		s.error(w, http.StatusInternalServerError, "could not read settings", err)
		return
	}
	cfg, err := llm.LoadConfig(settings)
	if err != nil {
		s.json(w, http.StatusOK, testResponse{Role: string(role), Error: err.Error()})
		return
	}

	chain := cfg.Roles[role]
	out := testResponse{Role: string(role)}
	if !chain.Primary.IsZero() {
		out.Provider = chain.Primary.Provider
		out.Model = chain.Primary.Model
	}

	result, err := s.Runner.TestCall(r.Context(), cfg, role)
	out.LatencyMS = result.LatencyMS
	if err != nil {
		out.Error = err.Error()
		// 200 with ok=false: a failing provider is a successful diagnosis, and
		// the UI wants to render the reason rather than handle an HTTP error.
		s.json(w, http.StatusOK, out)
		return
	}

	out.OK = true
	out.Reply = result.Reply
	out.Provider = result.Provider
	out.Model = result.Model
	out.TokensIn = result.TokensIn
	out.TokensOut = result.TokensOut
	s.json(w, http.StatusOK, out)
}

func stringList(raw string) []string {
	var out []string
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func intOf(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func floatOf(s string, def float64) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}
