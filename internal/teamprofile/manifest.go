package teamprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	SchemaVersion = 1
	Kind          = "ccr-team-profile"
	MaxBytes      = 1 << 20
	MaxProviders  = 256
	MaxModels     = 10_000
)

type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Kind          string     `json:"kind"`
	Providers     []Provider `json:"providers"`
	Models        []Model    `json:"models"`
}

type Provider struct {
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	BaseURL      string       `json:"base_url"`
	Protocol     string       `json:"protocol"`
	Mode         string       `json:"mode"`
	Capabilities Capabilities `json:"capabilities"`
	Credential   Credential   `json:"credential"`
}

type Capabilities struct {
	Tools          bool `json:"tools"`
	Streaming      bool `json:"streaming"`
	Thinking       bool `json:"thinking"`
	ModelDiscovery bool `json:"model_discovery"`
	CountTokens    bool `json:"count_tokens"`
}

type Credential struct {
	Required            bool   `json:"required"`
	EnvironmentVariable string `json:"environment_variable,omitempty"`
}

type Model struct {
	Alias         string `json:"alias"`
	Provider      string `json:"provider"`
	ProviderModel string `json:"provider_model"`
	Compatibility string `json:"compatibility"`
}

func Build(storedProviders []store.Provider, storedModels []store.Model) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Kind:          Kind,
		Providers:     make([]Provider, 0, len(storedProviders)),
		Models:        make([]Model, 0, len(storedModels)),
	}
	for index := range storedProviders {
		manifest.Providers = append(manifest.Providers, exportProvider(storedProviders[index]))
	}
	for index := range storedModels {
		model := &storedModels[index]
		manifest.Models = append(manifest.Models, Model{
			Alias:         model.Alias,
			Provider:      model.ProviderName,
			ProviderModel: model.ProviderModel,
			Compatibility: model.Status,
		})
	}
	sort.Slice(manifest.Providers, func(i, j int) bool {
		return manifest.Providers[i].Name < manifest.Providers[j].Name
	})
	sort.Slice(manifest.Models, func(i, j int) bool {
		return manifest.Models[i].Alias < manifest.Models[j].Alias
	})
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("building team profile: %w", err)
	}
	return manifest, nil
}

func exportProvider(provider store.Provider) Provider {
	required := provider.SecretRef != ""
	if profile, ok := (providers.Registry{}).Profile(provider.Type); ok {
		required = required || profile.RequiresAPIKey
	}
	credential := Credential{Required: required}
	if envName, ok := strings.CutPrefix(provider.SecretRef, "env:"); ok {
		credential.EnvironmentVariable = envName
	}
	return Provider{
		Name:     provider.Name,
		Type:     provider.Type,
		BaseURL:  provider.BaseURL,
		Protocol: provider.Protocol,
		Mode:     provider.Mode,
		Capabilities: Capabilities{
			Tools:          provider.SupportsTools,
			Streaming:      provider.SupportsStreaming,
			Thinking:       provider.SupportsThinking,
			ModelDiscovery: provider.SupportsModelDiscovery,
			CountTokens:    provider.SupportsCountTokens,
		},
		Credential: credential,
	}
}

func Encode(w io.Writer, manifest Manifest) error {
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("encoding team profile: %w", err)
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encoding team profile: %w", err)
	}
	return nil
}

func Decode(r io.Reader) (Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("reading team profile: %w", err)
	}
	if len(data) > MaxBytes {
		return Manifest{}, fmt.Errorf("team profile exceeds %d byte limit", MaxBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Manifest{}, fmt.Errorf("team profile is empty")
	}
	if err := rejectDuplicateFields(data); err != nil {
		return Manifest{}, fmt.Errorf("decoding team profile: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decoding team profile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Manifest{}, fmt.Errorf("decoding team profile: unexpected trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("decoding team profile: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validating team profile: %w", err)
	}
	return manifest, nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return consumeJSONObject(decoder)
	case '[':
		return consumeJSONArray(decoder)
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func consumeJSONObject(decoder *json.Decoder) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("object field name is not a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate JSON field %q", name)
		}
		seen[name] = struct{}{}
		if err := consumeJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func consumeJSONArray(decoder *json.Decoder) error {
	for decoder.More() {
		if err := consumeJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}
