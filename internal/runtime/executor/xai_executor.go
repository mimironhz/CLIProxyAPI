package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var (
	xaiDataTag  = []byte("data:")
	xaiEventTag = []byte("event:")
)

const (
	xaiImageHandlerType           = "openai-image"
	xaiVideoHandlerType           = "openai-video"
	xaiCustomToolType             = "custom"
	xaiFunctionToolType           = "function"
	xaiImageGenerationToolType    = "image_generation"
	xaiNamespaceToolType          = "namespace"
	xaiToolSearchType             = "tool_search"
	xaiWebSearchToolType          = "web_search"
	xaiXSearchToolType            = "x_search"
	xaiViewImageToolName          = "view_image"
	xaiInspectImageToolName       = "inspect_image"
	xaiViewImageDescription       = "View a local image file from the filesystem when visual inspection is needed. Use this for images already available on disk."
	xaiViewImagePathDescription   = "Local filesystem path to an image file."
	xaiViewImageDetailDescription = "Image detail level. Defaults to `high`; use `original` to preserve exact resolution."
	// Codex Desktop injects codex_app.automation_update with a large oneOf+$ref
	// schema. xAI's free/build Responses path accepts the HTTP request but never
	// emits SSE when that schema is present, so Desktop hangs on "thinking".
	xaiCodexAppNamespaceName    = "codex_app"
	xaiAutomationUpdateToolName = "automation_update"
	// Permissive placeholder for generic invalid root unions that xAI rejects.
	xaiSafeFunctionParameters = `{"type":"object","properties":{},"additionalProperties":true}`
	// Compact codex_app.automation_update schema: keep the mode discriminator and
	// common fields without the large oneOf+$ref tree that hangs xAI. An empty
	// additionalProperties object left the model free to invent nested args and
	// produced "mode: Invalid discriminator" loops in Desktop.
	xaiAutomationUpdateSafeParameters = `{"type":"object","properties":{"mode":{"type":"string","enum":["view","create","suggested_create","update","suggested_update","delete"],"description":"Operation discriminator. Must be a flat string; never nest other fields under mode."},"id":{"type":"string","description":"Existing automation id for view, update, suggested_update, or delete."},"name":{"type":"string","description":"Automation display name."},"prompt":{"type":"string","description":"Wake prompt for the automation."},"rrule":{"type":"string","description":"Recurrence rule, for example FREQ=MINUTELY;INTERVAL=15."},"status":{"type":"string","enum":["ACTIVE","PAUSED"]},"kind":{"type":"string","enum":["heartbeat","cron"],"description":"Prefer heartbeat unless the user asks for a standalone cron job."},"destination":{"type":"string","enum":["local","thread","worktree"],"description":"For heartbeat create, use thread for the current thread or provide targetThreadId."},"targetThreadId":{"type":"string","description":"Heartbeat target thread id. Required for create unless destination is thread."},"projectId":{"type":"string","description":"Cron project id from list_projects."},"model":{"type":"string"},"reasoningEffort":{"type":"string","enum":["none","minimal","low","medium","high","xhigh","max","ultra"]},"notificationPolicy":{"description":"Use failed_runs_only to mute success notifications, or null to unmute."},"executionEnvironment":{"type":"string","enum":["local","worktree"]}},"required":["mode"],"additionalProperties":true}`
	xaiImagesGenerationsPath          = "/images/generations"
	xaiImagesEditsPath                = "/images/edits"
	xaiDefaultImageEndpointPath       = xaiImagesGenerationsPath
	xaiVideosGenerationsPath          = "/videos/generations"
	xaiVideosEditsPath                = "/videos/edits"
	xaiVideosExtensionsPath           = "/videos/extensions"
	xaiVideosPath                     = "/videos"
	xaiIdempotencyKeyMetaKey          = "idempotency_key"
	xaiComposerModelPrefix            = "grok-composer-"
	xaiTokenAuthHeader                = "X-XAI-Token-Auth"
	xaiTokenAuthValue                 = "xai-grok-cli"
	xaiClientVersionHeader            = "x-grok-client-version"
	// Keep in sync with the current Grok CLI client version that chat-proxy expects.
	xaiClientVersionValue = "0.2.93"
	// xaiUsingAPIAttr enables the official API path for non-media HTTP chat.
	xaiUsingAPIAttr = "using_api"
)

// xaiXSearchToolJSON is the native X Search tool injected when enabled by config.
// Internal subtool traces are still filtered downstream when this tool is present.
var xaiXSearchToolJSON = []byte(`{"type":"x_search"}`)

// XAIExecutor is a stateless executor for xAI Grok's Responses API.
type XAIExecutor struct {
	cfg *config.Config
}

// NewXAIExecutor creates a new xAI executor.
func NewXAIExecutor(cfg *config.Config) *XAIExecutor {
	return &XAIExecutor{cfg: cfg}
}

// Identifier returns the provider identifier.
func (e *XAIExecutor) Identifier() string {
	return "xai"
}

// PrepareRequest injects xAI credentials into the outgoing HTTP request.
func (e *XAIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := xaiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects xAI credentials into the request and executes it.
func (e *XAIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("xai executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
