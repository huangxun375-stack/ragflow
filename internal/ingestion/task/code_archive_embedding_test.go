package task

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"ragflow/internal/common"
	"ragflow/internal/entity/models"
	componentpkg "ragflow/internal/ingestion/component"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"
)

// Keep provider calibration observations from unrelated tests out of this
// payload-preservation contract. This still uses the real cl100k counter.
func archiveTestEmbedder(t *testing.T, driver models.ModelDriver, limit int) *embedder {
	t.Helper()
	previousDelay := archiveLimitRetryDelay
	archiveLimitRetryDelay = time.Millisecond
	t.Cleanup(func() { archiveLimitRetryDelay = previousDelay })
	e := newLimitEmbedder(t, driver, limit)
	e.strictArchive = true
	e.limiterOnce.Do(func() { e.limiterVal = tokenizer.LimiterFor("", t.Name(), tokenizer.NewCalibration(1)) })
	return e
}

func TestArchiveEmbedderRefusesProviderLimitWithoutShrinking(t *testing.T) {
	driver := &tokenBoundDriver{maxTokens: 1}
	e := archiveTestEmbedder(t, driver, 2048)
	_, err := e.Encode(t.Context(), []string{"source code body must remain complete"})
	if codearchive.Reason(err) != "ZIP_TOKEN_LIMIT_EXCEEDED" || driver.rejects != 2 || driver.singleCalls != 0 {
		t.Fatalf("unexpected fallback: err=%v rejects=%d successes=%d", err, driver.rejects, driver.singleCalls)
	}
}

func TestArchiveEmbedderRefusesOversizedCodeBeforeProvider(t *testing.T) {
	driver := &stubDriver{}
	e := archiveTestEmbedder(t, driver, 128)
	_, err := e.Encode(t.Context(), []string{"valid code", strings.Repeat(" long code body", 4096)})
	if codearchive.Reason(err) != "ZIP_TOKEN_LIMIT_EXCEEDED" || len(driver.capturedTexts) != 0 {
		t.Fatalf("oversized input sent: err=%v", err)
	}
}

func TestArchiveEmbedderPreservesCompleteCode(t *testing.T) {
	driver := &stubDriver{}
	e := archiveTestEmbedder(t, driver, 2048)
	text := "// src/render.ts:L1-L1\nconst template = '<table><tr><td>code</td></tr></table>';"
	result, err := e.Encode(t.Context(), []string{text})
	if err != nil || len(result) != 1 || len(driver.capturedTexts) != 1 || driver.capturedTexts[0] != text {
		t.Fatalf("code was altered: err=%v sent=%v", err, driver.capturedTexts)
	}
}

// Only the provider boundary is faked; Encode, EmbedExact and token accounting
// all run normally, including validation of the provider's response count.
type archiveProviderDriver struct {
	stubDriver
	t       *testing.T
	texts   []string
	calls   [][]string
	respond func(context.Context, []string, int) (int, error)
}

func (d *archiveProviderDriver) Embed(ctx context.Context, _ *string, req models.EmbedRequest, _ *models.APIConfig, _ *models.EmbeddingConfig, usage *common.ModelUsage) ([]models.EmbeddingData, error) {
	d.t.Helper()
	d.calls = append(d.calls, slices.Clone(req.Texts))
	result := make([]models.EmbeddingData, len(req.Texts))
	for i, text := range req.Texts {
		original := slices.Index(d.texts, text)
		if original < 0 {
			d.t.Fatalf("provider received rewritten or truncated text: %q", text)
		}
		result[i] = models.EmbeddingData{Index: i, Embedding: []float64{float64(original), float64(len(text))}}
	}
	tokens, err := d.respond(ctx, req.Texts, len(d.calls))
	usage.InputTokens = tokens
	if err != nil {
		return nil, err
	}
	return result, nil
}

func archiveProviderTexts(n int) []string {
	texts := make([]string, n)
	for i := range texts {
		texts[i] = fmt.Sprintf("// src/render%d.ts:L1-L2\r\nconst template%d = '<table><tr><td>完整代码%s</td></tr></table>';\n", i, i, strings.Repeat(" body", i+1))
	}
	return texts
}

func assertArchiveVectors(t *testing.T, got []componentpkg.EmbeddingResult, texts []string) {
	t.Helper()
	if len(got) != len(texts) {
		t.Fatalf("vectors=%d, want %d", len(got), len(texts))
	}
	for i, text := range texts {
		if !slices.Equal(got[i].Vector, []float64{float64(i), float64(len(text))}) {
			t.Fatalf("vector %d out of order: %v", i, got[i].Vector)
		}
	}
}

func TestArchiveEmbedderIsolatesTransientBatchLimit(t *testing.T) {
	texts := archiveProviderTexts(5)
	driver := &archiveProviderDriver{t: t, texts: texts}
	driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
		if call == 1 {
			return 9999, errors.New(siliconflowOverLimitErr)
		}
		return 17 * call, nil
	}
	e := archiveTestEmbedder(t, driver, 2048)
	before := slices.Clone(texts)
	got, err := e.Encode(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveVectors(t, got, texts)
	if !slices.Equal(texts, before) || !reflect.DeepEqual(driver.calls, [][]string{texts, texts[:2], texts[2:]}) {
		t.Fatalf("unexpected inputs or split: %v", driver.calls)
	}
	for _, batch := range []struct{ start, end, usage int }{{0, 2, 34}, {2, 5, 51}} {
		total, largest, assigned := 0, batch.start, 0
		own := make([]int, len(texts))
		for i := batch.start; i < batch.end; i++ {
			own[i] = e.limiter().Counter().Count(texts[i])
			total += own[i]
			if own[i] > own[largest] {
				largest = i
			}
		}
		for i := batch.start; i < batch.end; i++ {
			assigned += batch.usage * own[i] / total
		}
		for i := batch.start; i < batch.end; i++ {
			want := batch.usage * own[i] / total
			if i == largest {
				want += batch.usage - assigned
			}
			if got[i].TokenCount != want {
				t.Fatalf("input %d tokens=%d, want %d from its successful sub-batch", i, got[i].TokenCount, want)
			}
		}
	}
	cal, key := e.limiter().Calibration()
	if _, _, _, observed := cal.Stats(key); observed {
		t.Fatal("archive isolation changed calibration")
	}
}

func TestArchiveEmbedderIsolatesPersistentProviderLimit(t *testing.T) {
	texts := archiveProviderTexts(5)
	providerErr := errors.New(siliconflowOverLimitErr)
	driver := &archiveProviderDriver{t: t, texts: texts}
	driver.respond = func(_ context.Context, batch []string, _ int) (int, error) {
		if slices.Contains(batch, texts[3]) {
			return 0, providerErr
		}
		return 7 * len(batch), nil
	}
	e := archiveTestEmbedder(t, driver, 2048)
	got, err := e.Encode(t.Context(), texts)
	wrapped := fmt.Errorf("tokenizer: encode: %w", err)
	if got != nil || codearchive.Reason(wrapped) != "ZIP_TOKEN_LIMIT_EXCEEDED" || !errors.Is(wrapped, providerErr) || !strings.Contains(wrapped.Error(), providerErr.Error()) {
		t.Fatalf("lost atomic failure or provider error: got=%v err=%v", got, wrapped)
	}
	wantCalls := [][]string{texts, texts[:2], texts[2:], texts[2:3], texts[3:], texts[3:4], texts[3:4]}
	if !reflect.DeepEqual(driver.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", driver.calls, wantCalls)
	}
}

func TestArchiveEmbedderRetriesTransientSingletonLimit(t *testing.T) {
	texts := archiveProviderTexts(1)
	driver := &archiveProviderDriver{t: t, texts: texts}
	driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
		if call == 1 {
			return 0, errors.New(siliconflowOverLimitErr)
		}
		return 19, nil
	}
	got, err := archiveTestEmbedder(t, driver, 2048).Encode(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveVectors(t, got, texts)
	if len(driver.calls) != 2 || got[0].TokenCount != 19 {
		t.Fatalf("calls=%d, result=%v", len(driver.calls), got)
	}
}

func TestArchiveEmbedderNonLimitErrorDoesNotIsolate(t *testing.T) {
	for _, phase := range []string{"initial", "split", "singleton-retry"} {
		t.Run(phase, func(t *testing.T) {
			texts := archiveProviderTexts(4)
			if phase == "singleton-retry" {
				texts = texts[:1]
			}
			providerErr := errors.New("SILICONFLOW API error: 401 Unauthorized, body: invalid credentials")
			driver := &archiveProviderDriver{t: t, texts: texts}
			driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
				if phase != "initial" && call == 1 {
					return 0, errors.New(siliconflowOverLimitErr)
				}
				return 0, providerErr
			}
			got, err := archiveTestEmbedder(t, driver, 2048).Encode(t.Context(), texts)
			wantCalls := 1
			if phase != "initial" {
				wantCalls = 2
			}
			if got != nil || err != providerErr || len(driver.calls) != wantCalls {
				t.Fatalf("got=%v err=%v calls=%d", got, err, len(driver.calls))
			}
		})
	}
}

func TestArchiveEmbedderIsolationHonorsCancellation(t *testing.T) {
	for _, phase := range []string{"before-call", "batch-rejected", "left-succeeded", "singleton-retry", "cooldown"} {
		t.Run(phase, func(t *testing.T) {
			freezeBackoff(t)
			texts := archiveProviderTexts(4)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantCalls := 1
			if phase == "before-call" {
				cancel()
				wantCalls = 0
			}
			if phase == "left-succeeded" {
				wantCalls = 2
			}
			if phase == "singleton-retry" {
				texts = texts[:1]
			}
			driver := &archiveProviderDriver{t: t, texts: texts}
			driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
				if phase == "left-succeeded" && call == 2 {
					cancel()
					return 17, nil
				}
				if phase != "left-succeeded" {
					cancel()
				}
				return 0, errors.New(siliconflowOverLimitErr)
			}
			e := archiveTestEmbedder(t, driver, 2048)
			if phase == "cooldown" {
				startCooldown(e.quotaKey(), time.Minute)
				cancel()
				wantCalls = 0
			}
			got, err := e.Encode(ctx, texts)
			if got != nil || err != context.Canceled || len(driver.calls) != wantCalls {
				t.Fatalf("got=%v err=%v calls=%d, want cancellation after %d calls", got, err, len(driver.calls), wantCalls)
			}
		})
	}
}

func TestArchiveEmbedderSingletonBackoffHonorsCancellation(t *testing.T) {
	texts := archiveProviderTexts(1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	driver := &archiveProviderDriver{t: t, texts: texts}
	driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
		if call > 1 {
			t.Fatal("singleton retried before its backoff elapsed")
		}
		timer := time.AfterFunc(20*time.Millisecond, cancel)
		t.Cleanup(func() { timer.Stop() })
		return 0, errors.New(siliconflowOverLimitErr)
	}
	e := archiveTestEmbedder(t, driver, 2048)
	archiveLimitRetryDelay = time.Minute
	started := time.Now()
	got, err := e.Encode(ctx, texts)
	if got != nil || err != context.Canceled || len(driver.calls) != 1 || time.Since(started) > time.Second {
		t.Fatalf("backoff ignored cancellation: got=%v err=%v calls=%d elapsed=%v", got, err, len(driver.calls), time.Since(started))
	}
}

func TestArchiveEmbedderRateLimitUsesExistingRetry(t *testing.T) {
	freezeBackoff(t)
	texts := archiveProviderTexts(4)
	driver := &archiveProviderDriver{t: t, texts: texts}
	driver.respond = func(_ context.Context, _ []string, call int) (int, error) {
		if call <= 2 {
			return 0, errors.New(tpmLimitErr)
		}
		return 28, nil
	}
	got, err := archiveTestEmbedder(t, driver, 2048).Encode(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveVectors(t, got, texts)
	if !reflect.DeepEqual(driver.calls, [][]string{texts, texts, texts}) {
		t.Fatalf("rate limit triggered isolation: %v", driver.calls)
	}
}

func TestArchiveEmbedderIsolationRequestBound(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 8, 33} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			texts := archiveProviderTexts(n)
			singleCalls := make(map[string]int)
			driver := &archiveProviderDriver{t: t, texts: texts}
			driver.respond = func(_ context.Context, batch []string, _ int) (int, error) {
				if len(batch) > 1 {
					return 0, errors.New(siliconflowOverLimitErr)
				}
				singleCalls[batch[0]]++
				if singleCalls[batch[0]] == 1 {
					return 0, errors.New(siliconflowOverLimitErr)
				}
				return 7, nil
			}
			e := archiveTestEmbedder(t, driver, 2048)
			got, err := e.Encode(t.Context(), texts)
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveVectors(t, got, texts)
			batches := (n + e.BatchSize() - 1) / e.BatchSize()
			if want := 3*n - batches; len(driver.calls) != want {
				t.Fatalf("calls=%d, want %d (binary tree plus one retry per leaf)", len(driver.calls), want)
			}
			for i, text := range texts {
				if singleCalls[text] != 2 || got[i].TokenCount != 7 {
					t.Fatalf("input %d: single calls=%d result=%v", i, singleCalls[text], got[i])
				}
			}
		})
	}
}
