package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	ChatModel           string   `json:"chat_model"`
	ChatFallback        []string `json:"chat_fallback"`

	ScheduleIngest     string `json:"schedule_ingest"`
	ScheduleSynthesize string `json:"schedule_synthesize"`
	ScheduleWeeklyDay  string `json:"schedule_weekly_day"`
	ScheduleWeeklyTime string `json:"schedule_weekly_time"`
	Timezone           string `json:"timezone"`

	DigestEnabled bool   `json:"digest_enabled"`
	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPUsername  string `json:"smtp_username"`
	// The env var NAME plus whether it resolves. Never the value -- same contract as
	// the provider registry.
	SMTPPasswordEnv      string `json:"smtp_password_env"`
	SMTPPasswordResolved bool   `json:"smtp_password_resolved"`
	DigestFrom           string `json:"digest_from"`
	DigestTo             string `json:"digest_to"`
	PingEnabled          bool   `json:"ping_enabled"`
	PingURL              string `json:"ping_url"`

	MaxCallsPerRun     int `json:"max_calls_per_run"`
	MaxCallsPerDay     int `json:"max_calls_per_day"`
	MaxChatCallsPerDay int `json:"max_chat_calls_per_day"`

	TranscriptsPerRun int     `json:"transcripts_per_run"`
	TranscriptLang    string  `json:"transcript_lang"`
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
	ChatModel           *string   `json:"chat_model"`
	ChatFallback        *[]string `json:"chat_fallback"`

	ScheduleIngest     *string `json:"schedule_ingest"`
	ScheduleSynthesize *string `json:"schedule_synthesize"`
	ScheduleWeeklyDay  *string `json:"schedule_weekly_day"`
	ScheduleWeeklyTime *string `json:"schedule_weekly_time"`

	DigestEnabled   *bool   `json:"digest_enabled"`
	SMTPHost        *string `json:"smtp_host"`
	SMTPPort        *int    `json:"smtp_port"`
	SMTPUsername    *string `json:"smtp_username"`
	SMTPPasswordEnv *string `json:"smtp_password_env"`
	DigestFrom      *string `json:"digest_from"`
	DigestTo        *string `json:"digest_to"`
	PingEnabled     *bool   `json:"ping_enabled"`
	PingURL         *string `json:"ping_url"`

	MaxCallsPerRun     *int `json:"max_calls_per_run"`
	MaxCallsPerDay     *int `json:"max_calls_per_day"`
	MaxChatCallsPerDay *int `json:"max_chat_calls_per_day"`

	TranscriptsPerRun *int     `json:"transcripts_per_run"`
	TranscriptLang    *string  `json:"transcript_lang"`
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
		ChatModel:           settings[store.KeyChatModel],
		ChatFallback:        stringList(settings[store.KeyChatFallback]),
		ScheduleIngest:      settings[store.KeyScheduleIngest],
		ScheduleSynthesize:  settings[store.KeyScheduleSynthesis],
		ScheduleWeeklyDay:   settings[store.KeyScheduleWeeklyDay],
		ScheduleWeeklyTime:  settings[store.KeyScheduleWeeklyTime],
		DigestEnabled:       settings[store.KeyDigestEnabled] == "1",
		SMTPHost:            settings[store.KeySMTPHost],
		SMTPPort:            intOf(settings[store.KeySMTPPort], 587),
		SMTPUsername:        settings[store.KeySMTPUsername],
		SMTPPasswordEnv:     settings[store.KeySMTPPasswordEnv],
		DigestFrom:          settings[store.KeyDigestFrom],
		DigestTo:            settings[store.KeyDigestTo],
		PingEnabled:         settings[store.KeyPingEnabled] == "1",
		PingURL:             settings[store.KeyPingURL],
		// Only whether it resolves, never the value. Same contract as provider keys.
		SMTPPasswordResolved: settings[store.KeySMTPPasswordEnv] == "" ||
			os.Getenv(settings[store.KeySMTPPasswordEnv]) != "",
		Timezone:           s.Env.Location.String(),
		MaxCallsPerRun:     intOf(settings[store.KeyMaxCallsPerRun], 200),
		MaxCallsPerDay:     intOf(settings[store.KeyMaxCallsPerDay], 500),
		MaxChatCallsPerDay: intOf(settings[store.KeyMaxChatCallsPerDay], 100),
		TranscriptsPerRun:  intOf(settings[store.KeyTranscriptsPerRun], 6),
		TranscriptLang:     settings[store.KeyTranscriptLang],
		HNMinPoints:        intOf(settings[store.KeyHNMinPoints], 25),
		SkyDays:            intOf(settings[store.KeySkyDays], 7),
		ClassifyBatchSize:  intOf(settings[store.KeyClassifyBatchSize], 30),
		HotThreshold:       intOf(settings[store.KeyHotThreshold], 3),
		MatchThreshold:     floatOf(settings[store.KeyMatchThreshold], 4),
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
	setBool := func(key string, v *bool) {
		if v != nil {
			if *v {
				updates[key] = "1"
			} else {
				updates[key] = "0"
			}
		}
	}
	setInt := func(key string, v *int) {
		if v != nil {
			updates[key] = strconv.Itoa(*v)
		}
	}

	setStr(store.KeyClassifierModel, req.ClassifierModel)
	setStr(store.KeySynthesizerModel, req.SynthesizerModel)
	setStr(store.KeyChatModel, req.ChatModel)
	setStr(store.KeyScheduleIngest, req.ScheduleIngest)
	setStr(store.KeyScheduleSynthesis, req.ScheduleSynthesize)
	setStr(store.KeyScheduleWeeklyDay, req.ScheduleWeeklyDay)
	setStr(store.KeyScheduleWeeklyTime, req.ScheduleWeeklyTime)
	setStr(store.KeySMTPHost, req.SMTPHost)
	setStr(store.KeySMTPUsername, req.SMTPUsername)
	setStr(store.KeySMTPPasswordEnv, req.SMTPPasswordEnv)
	setStr(store.KeyDigestFrom, req.DigestFrom)
	setStr(store.KeyDigestTo, req.DigestTo)
	setStr(store.KeyPingURL, req.PingURL)
	setInt(store.KeySMTPPort, req.SMTPPort)
	setBool(store.KeyDigestEnabled, req.DigestEnabled)
	setBool(store.KeyPingEnabled, req.PingEnabled)

	if req.ClassifierFallback != nil {
		updates[store.KeyClassifierFallback] = mustJSON(*req.ClassifierFallback)
	}
	if req.SynthesizerFallback != nil {
		updates[store.KeySynthFallback] = mustJSON(*req.SynthesizerFallback)
	}
	if req.ChatFallback != nil {
		updates[store.KeyChatFallback] = mustJSON(*req.ChatFallback)
	}

	setInt(store.KeyMaxCallsPerRun, req.MaxCallsPerRun)
	setInt(store.KeyMaxCallsPerDay, req.MaxCallsPerDay)
	setInt(store.KeyMaxChatCallsPerDay, req.MaxChatCallsPerDay)
	setInt(store.KeyTranscriptsPerRun, req.TranscriptsPerRun)
	setStr(store.KeyTranscriptLang, req.TranscriptLang)
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
	for _, key := range []string{
		store.KeyScheduleIngest, store.KeyScheduleSynthesis, store.KeyScheduleWeeklyTime,
	} {
		if v, ok := updates[key]; ok {
			if err := validateHHMM(v); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}

	// A weekday name, or empty to switch the round-up off.
	if v, ok := updates[store.KeyScheduleWeeklyDay]; ok && strings.TrimSpace(v) != "" {
		if !validWeekday(v) {
			return fmt.Errorf("schedule_weekly_day: want a weekday name like \"mon\", or empty to switch off")
		}
	}

	// A port that will not connect is worth rejecting at save time rather than
	// discovering when a digest silently never arrives.
	if v, ok := updates[store.KeySMTPPort]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("smtp_port: want a port between 1 and 65535")
		}
	}

	// Enabling delivery with nothing to deliver to is the one combination that fails
	// silently a week later, so it is refused now.
	merged2 := merged
	if merged2[store.KeyDigestEnabled] == "1" && strings.TrimSpace(merged2[store.KeyDigestTo]) == "" {
		return fmt.Errorf("digest_to: an email address is required to switch the digest on")
	}
	if merged2[store.KeyPingEnabled] == "1" && strings.TrimSpace(merged2[store.KeyPingURL]) == "" {
		return fmt.Errorf("ping_url: a webhook URL is required to switch pings on")
	}

	return nil
}

// validWeekday accepts the day names weeklySpec understands.
func validWeekday(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "sun", "sunday", "mon", "monday", "tue", "tuesday", "wed", "wednesday",
		"thu", "thursday", "fri", "friday", "sat", "saturday", "off", "never":
		return true
	}
	return false
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
	if role != llm.RoleClassifier && role != llm.RoleSynthesizer && role != llm.RoleChat {
		s.error(w, http.StatusBadRequest, "role must be classifier, synthesizer, or chat", nil)
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
