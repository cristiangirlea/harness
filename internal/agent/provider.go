package agent

import (
	"net/url"
	"slices"
	"strings"
)

type claudeProvider struct {
	enable string
	names  []string
}

var claudeProviders = map[string]claudeProvider{
	"bedrock": {"CLAUDE_CODE_USE_BEDROCK", []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "ANTHROPIC_BEDROCK_BASE_URL"}},
	"vertex":  {"CLAUDE_CODE_USE_VERTEX", []string{"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID", "ANTHROPIC_VERTEX_BASE_URL"}},
	"foundry": {"CLAUDE_CODE_USE_FOUNDRY", []string{"ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_BASE_URL"}},
	"gateway": {"", []string{"ANTHROPIC_BASE_URL"}},
}

var claudeProviderModels = []string{
	"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
}

var claudeProviderOverrides = []string{
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
}

func (p claudeProvider) apply(env []string) []string {
	env, _ = withoutEnvNames(env, claudeProviderOverrides)
	out := env[:0]
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(name)
		if strings.HasSuffix(key, "_BASE_URL") && slices.Contains(p.names, key) {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				continue
			}
			u.User = nil
			u.RawQuery, u.ForceQuery = "", false
			u.Fragment, u.RawFragment = "", ""
			kv = name + "=" + u.String()
		}
		out = append(out, kv)
	}
	if p.enable != "" {
		out = append(out, p.enable+"=1")
	}
	return out
}
