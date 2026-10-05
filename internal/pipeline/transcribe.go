package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// transcriptPromptChars is how much of a transcript the summarizer sees.
//
// A conference talk runs past 60,000 characters and the summary that comes out is
// no better for it -- the thesis is stated early and restated at the end, and the
// middle is examples. This bounds the cost of a stage that is otherwise unbounded
// by anything except how long people talk.
const transcriptPromptChars = 14_000

// TranscribeResult summarizes a transcript pass.
type TranscribeResult struct {
	Considered int `json:"considered"`
	Fetched    int `json:"fetched"`
	// NoCaptions is the ordinary case, not a failure: plenty of videos have none.
	NoCaptions int      `json:"no_captions"`
	Failed     int      `json:"failed"`
	Summarized int      `json:"summarized"`
	Problems   []string `json:"problems,omitempty"`
}

// Transcribe fetches captions for new videos and summarizes them.
//
// This is what makes a youtube_rss source worth having. Without it the classifier
// judges a video on its title and the channel's blurb, which is why the seeded
// video sources contributed almost nothing measurable -- the per-topic hit-rate
// view exists partly to have shown that.
//
// Deliberately placed between ingest and classify: the summary is written into
// items.summary, and it has to be there before the classifier reads the item or the
// whole exercise is a run late.
//
// Every failure is per-video and survivable. The caption path is undocumented and
// will break; when it does this stage should degrade to doing nothing, exactly as a
// dead feed does in ingest.
func Transcribe(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	cfg config.Runtime,
	log *slog.Logger,
) (TranscribeResult, error) {
	var result TranscribeResult

	if cfg.TranscriptsPerRun <= 0 {
		return result, nil // switched off
	}

	videos, err := db.VideosNeedingTranscripts(cfg.TranscriptsPerRun)
	if err != nil {
		return result, err
	}
	result.Considered = len(videos)
	if len(videos) == 0 {
		return result, nil
	}

	for i, v := range videos {
		reportProgress(ctx, i, len(videos), "videos")
		if err := ctx.Err(); err != nil {
			return result, err
		}

		videoID, ok := sources.VideoID(v.URL)
		if !ok {
			// Attributed to a YouTube source but not a video URL. Recorded as `none`
			// rather than an error so it is never reconsidered.
			_ = db.SaveTranscript(store.TranscriptRecord{
				ItemID: v.ItemID, Status: store.TranscriptNone,
				Err: "not a recognisable video URL",
			})
			result.NoCaptions++
			continue
		}

		transcript, err := sources.FetchTranscript(ctx, videoID, cfg.TranscriptLang)
		if err != nil {
			// No captions is the common answer and is permanent; anything else may be
			// worth retrying after a code change, so the two are stored differently.
			status := store.TranscriptError
			if isMissingCaptions(err) {
				status = store.TranscriptNone
				result.NoCaptions++
			} else {
				result.Failed++
				result.Problems = append(result.Problems,
					fmt.Sprintf("%s: %v", trimTitle(v.Title), err))
				log.Debug("transcript fetch failed", "item", v.ItemID, "err", err)
			}
			if err := db.SaveTranscript(store.TranscriptRecord{
				ItemID: v.ItemID, Status: status, Err: err.Error(),
			}); err != nil {
				return result, err
			}
			continue
		}
		result.Fetched++

		summary, err := summarizeTranscript(ctx, client, v.Title, transcript, log)
		if err != nil {
			if errors.Is(err, llm.ErrCapReached) || errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				// Store what was fetched so the next run only pays for the summary,
				// then stop: the cap applies to every remaining video too.
				if saveErr := db.SaveTranscript(store.TranscriptRecord{
					ItemID: v.ItemID, Status: store.TranscriptOK,
					Text: transcript.Text, Lang: transcript.Lang,
					Generated: transcript.Generated,
				}); saveErr != nil {
					return result, saveErr
				}
				return result, err
			}
			result.Problems = append(result.Problems,
				fmt.Sprintf("%s: summary failed: %v", trimTitle(v.Title), err))
			log.Warn("transcript summary failed", "item", v.ItemID, "err", err)
		}

		if err := db.SaveTranscript(store.TranscriptRecord{
			ItemID: v.ItemID, Status: store.TranscriptOK,
			Text: transcript.Text, Lang: transcript.Lang,
			Generated: transcript.Generated, Summary: summary,
		}); err != nil {
			return result, err
		}
		if summary != "" {
			result.Summarized++
		}
	}

	log.Info("transcripts complete",
		"considered", result.Considered, "fetched", result.Fetched,
		"summarized", result.Summarized, "no_captions", result.NoCaptions,
		"failed", result.Failed)
	return result, nil
}

// summarizeTranscript turns captions into something a classifier can judge.
//
// Runs on the CLASSIFIER's chain, logged as its own role. Summarizing a transcript
// down to three sentences is mechanical work, which is what the cheap-first role is
// for, and the separate log role keeps the cost visible instead of hiding inside
// classification's line in the cost log.
func summarizeTranscript(
	ctx context.Context,
	client *llm.Client,
	title string,
	transcript sources.Transcript,
	log *slog.Logger,
) (string, error) {
	text := transcript.Text
	if len(text) > transcriptPromptChars {
		text = sources.Truncate(text, transcriptPromptChars)
	}

	note := ""
	if transcript.Generated {
		// Worth telling the model: ASR mangles proper nouns, and a summary that
		// confidently repeats a misheard name is worse than one that hedges.
		note = "\nThese are automatic captions, so names and technical terms may be " +
			"transcribed incorrectly. Do not repeat a name you cannot make sense of.\n"
	}

	resp, err := client.CompleteAs(ctx, llm.RoleClassifier, llm.RoleTranscript, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: transcriptSystemPrompt},
			{Role: "user", Content: fmt.Sprintf(
				"VIDEO TITLE: %s\n%s\nTRANSCRIPT:\n%s\n\nSummarize it.", title, note, text)},
		},
		MaxTokens: 300,
	})
	if err != nil {
		return "", err
	}

	summary := strings.Join(strings.Fields(resp.Text), " ")
	// Long enough to be substance, short enough that the classifier's 400-character
	// truncation does not cut it mid-thought.
	if len(summary) > 600 {
		summary = sources.Truncate(summary, 600)
	}
	return summary, nil
}

const transcriptSystemPrompt = `You summarize the transcript of a video so a
relevance classifier can judge what it is about.

Write two or three sentences of plain prose. Lead with the specific claim, release,
or argument the video actually makes -- names, numbers and versions belong here.

Do not write "this video discusses" or "the speaker talks about". Do not describe
the format. Do not editorialize. Someone reading your two sentences should be able
to tell whether the subject matters to them.

Write in English even when the transcript is in another language.

No preamble, no bullet points, no code fences.`

// isMissingCaptions distinguishes the ordinary answer from a real failure.
//
// String matching on our own error text, not on YouTube's -- these are the messages
// FetchTranscript produces for "this video has no captions", and they are a
// permanent property of the video rather than something to retry.
func isMissingCaptions(err error) bool {
	msg := err.Error()
	for _, known := range []string{
		"no captions advertised",
		"no caption tracks",
		"no usable caption track",
		"caption track was empty",
	} {
		if strings.Contains(msg, known) {
			return true
		}
	}
	return false
}

func trimTitle(s string) string {
	if len(s) > 60 {
		return sources.Truncate(s, 60) + "…"
	}
	return s
}
