package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestGenerateVideoRejectsNonSixSecondBasicRequestsBeforeUpstream(t *testing.T) {
	observedDuration := 0
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rest/app-chat/conversations/new" {
			http.NotFound(writer, request)
			return
		}
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode video payload: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mediaGenInput, _ := payload["mediaGenInput"].(map[string]any)
		textToVideo, _ := mediaGenInput["textToVideo"].(map[string]any)
		duration, _ := textToVideo["duration"].(float64)
		observedDuration = int(duration)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/final.mp4"}}}}`+"\n\n")
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test",
		VideoTimeoutSeconds: 5, FreeVideoDurationCap: 4,
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)

	credential := account.Credential{
		ID: 1, Provider: account.ProviderWeb, WebTier: account.WebTierBasic,
		EncryptedAccessToken: encryptedToken,
	}
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential,
		Prompt:     "test", Duration: 6,
	})
	if err != nil || result.URL != "https://assets.grok.com/videos/final.mp4" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if observedDuration != 6 {
		t.Fatalf("Basic Web request upstream duration = %d, want 6", observedDuration)
	}

	for _, seconds := range []int{1, 4, 5, 7, 10, 15, 16} {
		_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
			Credential: credential, Prompt: "test", Duration: seconds,
			ImageURL: "https://example.invalid/reference.png",
		})
		var stageError *provider.VideoStageError
		if !errors.As(err, &stageError) || stageError.Stage != provider.VideoStagePrepare {
			t.Fatalf("duration %d: error = %v, want prepare-stage validation failure", seconds, err)
		}
	}
	if requestCount != 1 {
		t.Fatalf("invalid duration reached upstream; request count = %d, want 1", requestCount)
	}
}

func TestGenerateVideoRejectsShortNonFreeRequestsBeforePreparation(t *testing.T) {
	adapter := &Adapter{}
	for _, tier := range []account.WebTier{account.WebTierAuto, account.WebTierSuper, account.WebTierHeavy, ""} {
		for _, seconds := range []int{1, 5, 6} {
			_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
				Credential: account.Credential{WebTier: tier}, Duration: seconds,
			})
			var stageError *provider.VideoStageError
			if !errors.As(err, &stageError) || stageError.Stage != provider.VideoStagePrepare {
				t.Fatalf("tier=%q duration=%d: error=%v", tier, seconds, err)
			}
		}
	}
}
