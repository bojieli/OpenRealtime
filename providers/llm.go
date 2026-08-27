package providers

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	anthropicadapter "github.com/bojieli/OpenRealtime/adapters/anthropic"
	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// LLM is one language-model provider.
type LLM struct {
	Common
	// FastModel and SlowModel are the default identities for the two phases.
	//
	// They are empty for a marketplace - OpenRouter, Groq, Together,
	// SiliconFlow - because a marketplace serves other people's models and
	// there is no default that is right for anyone. A provider that serves its
	// own models has one, and it is a hint: see Reviewed.
	FastModel string
	SlowModel string
	// Reasoning is the field this endpoint reads the reasoning switch from.
	Reasoning openaicompat.ReasoningControl
	// EffortNames overrides the portable effort vocabulary.
	EffortNames map[continuation.Effort]string
	// DisabledEffort is the effort level that means "do not reason".
	DisabledEffort string
	// MaxTokensField names the output-limit field.
	MaxTokensField openaicompat.MaxTokensField
	// ReasoningDelta names the streaming field carrying reasoning text.
	ReasoningDelta openaicompat.ReasoningDeltaField
	// Vision declares that the default models accept images.
	Vision bool
	// Headers are provider-required request headers.
	Headers map[string]string
}

// openAIEfforts is the effort vocabulary OpenAI defined and several endpoints
// copied. It omits minimal deliberately: OpenAI spells "do not reason" as
// none, which is the disabled effort rather than a level, so asking for
// minimal effort from a reasoning continuation is a configuration error rather
// than something to round off.
var openAIEfforts = map[continuation.Effort]string{
	continuation.EffortLow: "low", continuation.EffortMedium: "medium", continuation.EffortHigh: "high",
}

// llmCatalog is the language-model catalogue.
//
// Order is by how likely a reader is to be looking for the entry, not
// alphabetical: the frontier labs, then the open-weight labs, then the
// marketplaces, then what runs on the operator's own machine.
var llmCatalog = []LLM{
	{
		Common: Common{
			Name: "openai", Label: "OpenAI", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.openai.com/v1", Auth: AuthBearer,
			KeyEnv: []string{"OPENAI_API_KEY"},
			Notes: "OpenAI steers reasoning with tools to its Responses API, which this adapter " +
				"does not speak; if the slow phase is refused, lower -slow-effort or use another provider.",
		},
		FastModel: "gpt-5.6-luna", SlowModel: "gpt-5.6-sol",
		Reasoning: openaicompat.ReasoningControlEffort, DisabledEffort: "none",
		EffortNames: openAIEfforts, MaxTokensField: openaicompat.MaxTokensCompletion, Vision: true,
	},
	{
		Common: Common{
			Name: "anthropic", Aliases: []string{"claude"}, Label: "Anthropic",
			Dialect: DialectAnthropicMessages, BaseURL: anthropicadapter.DefaultBaseURL,
			Auth: AuthAnthropic, KeyEnv: []string{"ANTHROPIC_API_KEY"},
		},
		FastModel: "claude-haiku-4-5", SlowModel: "claude-opus-5",
		EffortNames: map[continuation.Effort]string{
			continuation.EffortLow: "low", continuation.EffortMedium: "medium",
			continuation.EffortHigh: "high",
		},
		Vision: true,
	},
	{
		Common: Common{
			Name: "google", Aliases: []string{"gemini"}, Label: "Google Gemini",
			Dialect: DialectGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta",
			Auth: AuthQuery, KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		},
		// Both phases are the same model, and that is the reference
		// configuration rather than an oversight: what separates fast from
		// slow here is reasoning effort and tool authority, not model family.
		// There is no gemini-3.5-pro - the 3.5 family is flash, flash-lite,
		// and a live translation preview - so naming one made the background
		// reasoner return 404 on every turn while the voice kept answering,
		// which looks like a working deployment until somebody notices that
		// nothing is ever reasoned about or acted on.
		FastModel: gemini.DefaultModel, SlowModel: gemini.DefaultModel, Vision: true,
	},
	{
		Common: Common{
			Name: "google-openai", Aliases: []string{"gemini-openai"},
			Label: "Google Gemini (OpenAI compatibility)", Dialect: DialectOpenAIChat,
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			Auth:    AuthBearer, KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			Notes: "Use the native google entry unless a compatibility layer is required; " +
				"thought signatures are not preserved here.",
		},
		FastModel: gemini.DefaultModel, SlowModel: gemini.DefaultModel,
		Reasoning: openaicompat.ReasoningControlEffort, DisabledEffort: "none",
		EffortNames: openAIEfforts, Vision: true,
	},
	{
		Common: Common{
			Name: "xai", Aliases: []string{"grok"}, Label: "xAI", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.x.ai/v1", Auth: AuthBearer, KeyEnv: []string{"XAI_API_KEY"},
			Notes: "Grok reasons on its own; the effort flag is not sent.",
		},
		FastModel: "grok-4.6-fast", SlowModel: "grok-4.6",
		Reasoning: openaicompat.ReasoningControlNone, Vision: true,
	},
	{
		Common: Common{
			Name: "deepseek", Label: "DeepSeek", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.deepseek.com/v1", Auth: AuthBearer,
			KeyEnv: []string{"DEEPSEEK_API_KEY"},
		},
		FastModel: "deepseek-v4-flash", SlowModel: "deepseek-v4-pro",
		Reasoning: openaicompat.ReasoningControlEffort, DisabledEffort: "none",
		EffortNames: openAIEfforts, ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "deepseek-anthropic", Label: "DeepSeek (Anthropic compatibility)",
			Dialect: DialectAnthropicMessages, BaseURL: "https://api.deepseek.com/anthropic",
			Auth: AuthAnthropic, KeyEnv: []string{"DEEPSEEK_API_KEY"},
			Notes: "DeepSeek's Messages-API endpoint, for a deployment standardised on that dialect.",
		},
		FastModel: "deepseek-v4-flash", SlowModel: "deepseek-v4-pro",
	},
	{
		Common: Common{
			Name: "zhipu", Aliases: []string{"glm", "bigmodel"}, Label: "Zhipu GLM",
			Dialect: DialectOpenAIChat, BaseURL: "https://open.bigmodel.cn/api/paas/v4",
			Auth: AuthBearer, KeyEnv: []string{"ZHIPUAI_API_KEY", "GLM_API_KEY"},
		},
		FastModel: "glm-5.2-air", SlowModel: "glm-5.2",
		Reasoning:      openaicompat.ReasoningControlThinkingObject,
		ReasoningDelta: openaicompat.ReasoningContentField, Vision: true,
	},
	{
		Common: Common{
			Name: "z-ai", Aliases: []string{"zhipu-international"}, Label: "Z.ai (Zhipu international)",
			Dialect: DialectOpenAIChat, BaseURL: "https://api.z.ai/api/paas/v4",
			Auth: AuthBearer, KeyEnv: []string{"ZAI_API_KEY", "ZHIPUAI_API_KEY"},
		},
		FastModel: "glm-5.2-air", SlowModel: "glm-5.2",
		Reasoning:      openaicompat.ReasoningControlThinkingObject,
		ReasoningDelta: openaicompat.ReasoningContentField, Vision: true,
	},
	{
		Common: Common{
			Name: "minimax", Label: "MiniMax", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.minimaxi.com/v1", Auth: AuthBearer,
			KeyEnv: []string{"MINIMAX_API_KEY"},
			Notes:  "Mainland-China accounts use https://api.minimax.chat/v1 instead.",
		},
		FastModel: "MiniMax-M2.7", SlowModel: "MiniMax-M3",
		ReasoningDelta: openaicompat.ReasoningContentField, Vision: true,
	},
	{
		Common: Common{
			Name: "moonshot", Aliases: []string{"kimi"}, Label: "Moonshot Kimi",
			Dialect: DialectOpenAIChat, BaseURL: "https://api.moonshot.ai/v1", Auth: AuthBearer,
			KeyEnv: []string{"MOONSHOT_API_KEY"},
			Notes:  "Mainland-China accounts use https://api.moonshot.cn/v1 instead.",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "dashscope", Aliases: []string{"qwen", "alibaba"}, Label: "Alibaba DashScope (Qwen)",
			Dialect: DialectOpenAIChat,
			BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Auth: AuthBearer,
			KeyEnv: []string{"DASHSCOPE_API_KEY"},
			Notes: "Outside mainland China use " +
				"https://dashscope-intl.aliyuncs.com/compatible-mode/v1.",
		},
		FastModel: "qwen-flash", SlowModel: "qwen-max",
		Reasoning:      openaicompat.ReasoningControlEnableThinking,
		ReasoningDelta: openaicompat.ReasoningContentField, Vision: true,
	},
	{
		Common: Common{
			Name: "mistral", Label: "Mistral", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.mistral.ai/v1", Auth: AuthBearer,
			KeyEnv: []string{"MISTRAL_API_KEY"},
		},
		FastModel: "mistral-small-latest", SlowModel: "mistral-large-latest", Vision: true,
	},
	{
		Common: Common{
			Name: "perplexity", Label: "Perplexity", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.perplexity.ai", Auth: AuthBearer,
			KeyEnv: []string{"PERPLEXITY_API_KEY"},
		},
		FastModel: "sonar", SlowModel: "sonar-reasoning-pro",
	},
	{
		Common: Common{
			Name: "ark", Aliases: []string{"doubao", "volcengine"}, Label: "Volcengine Ark (Doubao)",
			Dialect: DialectOpenAIChat, BaseURL: "https://ark.cn-beijing.volces.com/api/v3",
			Auth: AuthBearer, KeyEnv: []string{"ARK_API_KEY"},
			Notes: "Ark addresses models by endpoint ID; set the model flags explicitly.",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "qianfan", Aliases: []string{"baidu", "ernie"}, Label: "Baidu Qianfan (ERNIE)",
			Dialect: DialectOpenAIChat, BaseURL: "https://qianfan.baidubce.com/v2",
			Auth: AuthBearer, KeyEnv: []string{"QIANFAN_API_KEY"},
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "openrouter", Label: "OpenRouter", Dialect: DialectOpenAIChat,
			BaseURL: "https://openrouter.ai/api/v1", Auth: AuthBearer,
			KeyEnv: []string{"OPENROUTER_API_KEY"},
			Notes:  "A marketplace: name the model as vendor/model, for example anthropic/claude-opus-5.",
		},
		Reasoning: openaicompat.ReasoningControlEffort, DisabledEffort: "none",
		EffortNames: openAIEfforts, ReasoningDelta: openaicompat.ReasoningField,
		Headers: map[string]string{
			"HTTP-Referer": "https://github.com/bojieli/OpenRealtime",
			"X-Title":      "OpenRealtime",
		},
	},
	{
		Common: Common{
			Name: "siliconflow", Label: "SiliconFlow", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.siliconflow.cn/v1", Auth: AuthBearer,
			KeyEnv: []string{"SILICONFLOW_API_KEY"},
			Notes: "A marketplace: name the model as org/model. " +
				"Outside mainland China use https://api.siliconflow.com/v1.",
		},
		Reasoning:      openaicompat.ReasoningControlEnableThinking,
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "groq", Label: "Groq", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.groq.com/openai/v1", Auth: AuthBearer,
			KeyEnv: []string{"GROQ_API_KEY"},
			Notes:  "A marketplace, and the fastest one; name an open-weight model explicitly.",
		},
		ReasoningDelta: openaicompat.ReasoningField,
	},
	{
		Common: Common{
			Name: "cerebras", Label: "Cerebras", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.cerebras.ai/v1", Auth: AuthBearer,
			KeyEnv: []string{"CEREBRAS_API_KEY"},
			Notes:  "A marketplace; name an open-weight model explicitly.",
		},
	},
	{
		Common: Common{
			Name: "together", Label: "Together AI", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.together.xyz/v1", Auth: AuthBearer,
			KeyEnv: []string{"TOGETHER_API_KEY"},
			Notes:  "A marketplace; name the model as org/model.",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "fireworks", Label: "Fireworks AI", Dialect: DialectOpenAIChat,
			BaseURL: "https://api.fireworks.ai/inference/v1", Auth: AuthBearer,
			KeyEnv: []string{"FIREWORKS_API_KEY"},
			Notes:  "A marketplace; name the model as accounts/fireworks/models/...",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "nvidia", Aliases: []string{"nim"}, Label: "NVIDIA NIM", Dialect: DialectOpenAIChat,
			BaseURL: "https://integrate.api.nvidia.com/v1", Auth: AuthBearer,
			KeyEnv: []string{"NVIDIA_API_KEY"},
			Notes:  "A marketplace; name the model as org/model.",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "azure-openai", Aliases: []string{"azure"}, Label: "Azure OpenAI",
			Dialect: DialectOpenAIChat, Auth: AuthBearer,
			KeyEnv: []string{"AZURE_OPENAI_API_KEY"},
			Notes: "Set the base URL to https://RESOURCE.openai.azure.com/openai/v1 " +
				"and the model to your deployment name.",
		},
		Reasoning: openaicompat.ReasoningControlEffort, DisabledEffort: "none",
		EffortNames: openAIEfforts, MaxTokensField: openaicompat.MaxTokensCompletion, Vision: true,
	},
	{
		Common: Common{
			Name: "vllm", Label: "vLLM", Dialect: DialectOpenAIChat,
			BaseURL: openaicompat.DefaultBaseURL, Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_LOCAL_API_KEY"},
			Notes:  "Whatever the server was started with; set the model flags to match.",
		},
		Reasoning:      openaicompat.ReasoningControlTemplateKwargs,
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "sglang", Label: "SGLang", Dialect: DialectOpenAIChat,
			BaseURL: "http://127.0.0.1:30000/v1", Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_LOCAL_API_KEY"},
		},
		Reasoning:      openaicompat.ReasoningControlTemplateKwargs,
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "ollama", Label: "Ollama", Dialect: DialectOpenAIChat,
			BaseURL: "http://127.0.0.1:11434/v1", Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_LOCAL_API_KEY"},
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "llama-cpp", Aliases: []string{"llamacpp"}, Label: "llama.cpp server",
			Dialect: DialectOpenAIChat, BaseURL: "http://127.0.0.1:8080/v1",
			Auth: AuthNone, Local: true, KeyEnv: []string{"OPENREALTIME_LOCAL_API_KEY"},
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "lm-studio", Aliases: []string{"lmstudio"}, Label: "LM Studio",
			Dialect: DialectOpenAIChat, BaseURL: "http://127.0.0.1:1234/v1",
			Auth: AuthNone, Local: true, KeyEnv: []string{"OPENREALTIME_LOCAL_API_KEY"},
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
	{
		Common: Common{
			Name: "openai-compatible", Aliases: []string{"custom"}, Label: "Any OpenAI-compatible endpoint",
			Dialect: DialectOpenAIChat, BaseURL: openaicompat.DefaultBaseURL, Auth: AuthBearer,
			// Its default endpoint is loopback, so it must run with nothing
			// configured: this is what `openrealtime serve` with no flags
			// selects. A credential is sent when the variable is set, which is
			// what makes the same entry usable against a hosted endpoint.
			Local:  true,
			KeyEnv: []string{"OPENREALTIME_LLM_API_KEY"},
			Notes:  "The escape hatch: point the base URL anywhere that speaks Chat Completions.",
		},
		ReasoningDelta: openaicompat.ReasoningContentField,
	},
}

// LLMs returns the catalogue in listing order.
func LLMs() []LLM { return slices.Clone(llmCatalog) }

// LookupLLM resolves a provider name or alias.
func LookupLLM(name string) (LLM, error) {
	normalised := normalise(name)
	for _, entry := range llmCatalog {
		if entry.matches(normalised) {
			return entry, nil
		}
	}
	return LLM{}, &UnknownError{
		Role: RoleLLM, Name: name,
		Known: names(llmCatalog, func(entry LLM) Common { return entry.Common }),
	}
}

// Reason is what a phase asks of a provider's reasoning.
type Reason string

const (
	// ReasonOff asks the provider not to reason. It is what the voice wants:
	// the fast phase answers the question that was asked, immediately.
	ReasonOff Reason = "off"
	// ReasonOn asks the provider to reason.
	ReasonOn Reason = "on"
	// ReasonDefault says nothing, leaving the endpoint's own default.
	ReasonDefault Reason = "default"
)

// LLMRequest is one resolved provider construction.
type LLMRequest struct {
	// Provider is the catalogue name.
	Provider string
	// Model overrides the catalogue default. It is required for an entry that
	// has none.
	Model string
	// BaseURL overrides the catalogue endpoint.
	BaseURL string
	// APIKey overrides the environment lookup.
	APIKey string
	// KeyEnv overrides which environment variable is read. It exists so a
	// deployment can run two profiles of one provider on separate keys.
	KeyEnv          string
	Phase           trajectory.Phase
	Effort          continuation.Effort
	ToolAuthority   continuation.ToolAuthority
	SpeechAuthority continuation.SpeechAuthority
	Reason          Reason
	// Vision overrides whether images are sent. Nil takes the catalogue's
	// declaration, which is about the default models rather than the one that
	// was actually configured.
	Vision *bool
	// RetainReasoning asks a provider that can return its reasoning to do so.
	RetainReasoning bool
	// Temperature is an explicit sampling temperature. Nil leaves the
	// provider's default intact; a pointer is required because zero is the
	// deterministic setting rather than an omitted value.
	Temperature    *float64
	RequestTimeout time.Duration
}

// NewLLM builds the configured provider.
//
// Everything a phase decides - authority over tools, authority over speech,
// whether to reason - is passed in rather than inferred from the catalogue.
// The catalogue knows how to reach a provider; the arrangement knows what the
// provider is for, and conflating the two is how a fast provider ends up with
// tool authority because its vendor happened to support tools.
func NewLLM(request LLMRequest) (continuation.Provider, error) {
	entry, err := LookupLLM(request.Provider)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = entry.defaultModel(request.Phase)
	}
	if model == "" {
		return nil, fmt.Errorf(
			"provider %q has no default %s model; pass a model explicitly (openrealtime providers -probe %s lists what it serves)",
			entry.Name, request.Phase, entry.Name)
	}
	baseURL := strings.TrimSpace(request.BaseURL)
	if baseURL == "" {
		baseURL = entry.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider %q has no default endpoint; pass a base URL", entry.Name)
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return nil, fmt.Errorf("provider %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}
	vision := entry.Vision
	if request.Vision != nil {
		vision = *request.Vision
	}

	switch entry.Dialect {
	case DialectAnthropicMessages:
		return anthropicadapter.New(anthropicadapter.Config{
			APIKey: key, Model: model, BaseURL: baseURL, Provider: entry.Name,
			Phase: request.Phase, Effort: request.Effort,
			ToolAuthority: request.ToolAuthority, SpeechAuthority: request.SpeechAuthority,
			Vision:      &vision,
			Thinking:    anthropicThinking(request.Reason),
			EffortNames: entry.EffortNames,
			// Reasoning text is only worth asking for when the runtime keeps
			// it. Current models return empty thinking blocks otherwise, and
			// asking for a summary that is discarded is paid-for latency.
			IncludeThoughts: request.RetainReasoning && request.Reason != ReasonOff,
			Temperature:     request.Temperature,
			RequestTimeout:  request.RequestTimeout,
		})
	case DialectGemini:
		return gemini.New(gemini.Config{
			APIKey: key, Model: model, Endpoint: baseURL, Phase: request.Phase,
			Effort: request.Effort, ToolAuthority: request.ToolAuthority,
			SpeechAuthority: request.SpeechAuthority,
			IncludeThoughts: request.RetainReasoning && request.Reason != ReasonOff,
			Temperature:     request.Temperature,
			RequestTimeout:  request.RequestTimeout,
		})
	case DialectOpenAIChat:
		thinking := openaicompat.ThinkingAuto
		switch request.Reason {
		case ReasonOff:
			thinking = openaicompat.ThinkingDisabled
		case ReasonOn:
			thinking = openaicompat.ThinkingEnabled
		}
		reasoning := entry.Reasoning
		if reasoning == "" {
			reasoning = openaicompat.ReasoningControlNone
		}
		return openaicompat.New(openaicompat.Config{
			APIKey: key, Model: model, BaseURL: baseURL, Provider: entry.Name,
			Phase: request.Phase, Effort: request.Effort,
			ToolAuthority: request.ToolAuthority, SpeechAuthority: request.SpeechAuthority,
			Vision: vision, ThinkingMode: thinking, ReasoningControl: reasoning,
			EffortNames: entry.EffortNames, DisabledEffort: entry.DisabledEffort,
			MaxTokensField:          entry.MaxTokensField,
			ReasoningDeltaField:     entry.ReasoningDelta,
			DisableReasoningCapture: request.Reason == ReasonOff,
			Headers:                 entry.Headers,
			Temperature:             request.Temperature,
			RequestTimeout:          request.RequestTimeout,
		})
	default:
		return nil, fmt.Errorf("provider %q has no language-model adapter for dialect %q",
			entry.Name, entry.Dialect)
	}
}

func anthropicThinking(reason Reason) anthropicadapter.Thinking {
	switch reason {
	case ReasonOff:
		return anthropicadapter.ThinkingDisabled
	case ReasonOn:
		return anthropicadapter.ThinkingAdaptive
	default:
		return anthropicadapter.ThinkingOmitted
	}
}

// defaultModel returns the catalogue default for a phase.
func (entry LLM) defaultModel(phase trajectory.Phase) string {
	if phase == trajectory.PhaseFast {
		return entry.FastModel
	}
	return entry.SlowModel
}

// credential resolves the key, honouring an explicit variable override.
func (common Common) credential(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(os.Getenv(override))
	}
	return common.keyFromEnvironment()
}

// credentialHint names the variables that would have satisfied the lookup.
func (common Common) credentialHint(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	if len(common.KeyEnv) == 0 {
		return "an API key environment variable"
	}
	return strings.Join(common.KeyEnv, " or ")
}
