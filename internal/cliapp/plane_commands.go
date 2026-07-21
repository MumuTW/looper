package cliapp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/planeprotocol"
	"github.com/spf13/cobra"
)

const planeLinkMediaType = "application/vnd.looper.link-request+cbor;v=1"

type planeLinkIdentity struct {
	ID string `json:"id"`
}

type planeLinkBinding struct {
	ID          string `json:"id"`
	MemberID    string `json:"member_id"`
	NodeID      string `json:"node_id"`
	State       string `json:"state"`
	KeyRevision uint64 `json:"revision"`
}

func (r *commandRuntime) planeLink(cmd *cobra.Command, args []string) error {
	loaded, providerIndex, provider, token, strictBaseURL, err := r.planeCommandContext(args, getStringFlag(cmd, "strict-base-url"))
	if err != nil {
		return err
	}
	homeDir, err := r.homeDir()
	if err != nil {
		return err
	}
	networkState, err := networkclient.LoadState(networkclient.DefaultStatePath(homeDir))
	if err != nil {
		return fmt.Errorf("plane link requires a joined loopernet network: %w", err)
	}
	if provider.StrictDispatch != nil && strings.TrimSpace(provider.StrictDispatch.BindingID) != "" {
		return errors.New("Plane provider already has a strict binding; use `looper plane enable` after approval")
	}

	privateKeyFile := filepath.Join(homeDir, ".looper", "runtime", "plane-"+safeFileComponent(provider.ID)+".pem")
	if _, readErr := r.readFile(privateKeyFile); readErr == nil {
		return fmt.Errorf("refusing to replace existing Plane private key %s", privateKeyFile)
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("inspect Plane private key: %w", readErr)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}

	user := planeLinkIdentity{}
	if err := r.planeJSONRequest(cmd.Context(), provider.BaseURL, token, http.MethodGet, "users/me", "", nil, &user); err != nil {
		return fmt.Errorf("read current Plane identity: %w", err)
	}
	workspace := planeLinkIdentity{}
	if err := r.planeJSONRequest(cmd.Context(), provider.BaseURL, token, http.MethodGet, "workspaces/"+url.PathEscape(*provider.Workspace), "", nil, &workspace); err != nil {
		return fmt.Errorf("read Plane workspace: %w", err)
	}
	workspaceID, err := parsePlaneProtocolUUID(workspace.ID)
	if err != nil {
		return fmt.Errorf("Plane workspace id: %w", err)
	}
	projectID, err := parsePlaneProtocolUUID(*provider.ProjectID)
	if err != nil {
		return fmt.Errorf("Plane project id: %w", err)
	}
	memberID, err := parsePlaneProtocolUUID(user.ID)
	if err != nil {
		return fmt.Errorf("Plane member id: %w", err)
	}
	publicHash := sha256.Sum256(publicKey)
	challengeResponse, err := networkclient.New(networkState.URL, networkState.NodeToken, r.httpClient()).LinkChallenge(
		cmd.Context(),
		protocol.LinkChallengeRequest{
			PublicKeySHA256: base64.RawURLEncoding.EncodeToString(publicHash[:]),
			Audience:        "plane:" + workspace.ID,
		},
	)
	if err != nil {
		return fmt.Errorf("request loopernet Plane challenge: %w", err)
	}
	challengeEnvelope, err := base64.RawURLEncoding.Strict().DecodeString(challengeResponse.Challenge)
	if err != nil {
		return fmt.Errorf("decode loopernet challenge: %w", err)
	}
	envelope, err := planeprotocol.DecodeEnvelope(challengeEnvelope)
	if err != nil {
		return fmt.Errorf("decode loopernet challenge envelope: %w", err)
	}
	challenge, err := planeprotocol.DecodeLinkChallenge(envelope.Payload)
	if err != nil {
		return err
	}
	if challenge.NetworkID != networkState.NetworkID || challenge.NodeID != networkState.NodeID || challenge.PublicKeySHA256 != publicHash || challenge.Audience != "plane:"+workspace.ID {
		return errors.New("loopernet challenge does not match this Node and Plane workspace")
	}
	challengeDigest, err := planeprotocol.DomainDigest(planeprotocol.LinkChallengeProfile, envelope.Payload)
	if err != nil {
		return err
	}
	var publicKeyFixed [32]byte
	copy(publicKeyFixed[:], publicKey)
	proofPayload, err := planeprotocol.EncodeLinkProof(planeprotocol.LinkProof{
		ChallengeSHA256: challengeDigest,
		PlaneWorkspace:  workspaceID,
		PlaneProject:    projectID,
		MemberID:        memberID,
		PublicKey:       publicKeyFixed,
	})
	if err != nil {
		return err
	}
	proofEnvelope, err := planeprotocol.SignEnvelope(privateKey, planeprotocol.LinkProofProfile, 0, proofPayload)
	if err != nil {
		return err
	}
	linkBody, err := planeprotocol.EncodeLinkRequest(challengeEnvelope, proofEnvelope)
	if err != nil {
		return err
	}
	linkPath := fmt.Sprintf(
		"/api/workspaces/%s/projects/%s/looper/bindings/link/",
		url.PathEscape(*provider.Workspace),
		url.PathEscape(*provider.ProjectID),
	)
	binding := planeLinkBinding{}
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPost, linkPath, planeLinkMediaType, linkBody, &binding); err != nil {
		return fmt.Errorf("create Plane Node binding: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(privateKeyFile), 0o700); err != nil {
		return err
	}
	if err := r.writeFile(privateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return err
	}
	strict := config.PlaneStrictDispatchConfig{
		Enabled: false, BaseURL: strictBaseURL, NodeID: networkState.NodeID,
		BindingID: binding.ID, KeyRevision: 1, PrivateKeyFile: privateKeyFile,
	}
	if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
		_ = r.removeFile(privateKeyFile)
		return err
	}
	output := map[string]any{
		"providerId": provider.ID, "bindingId": binding.ID, "bindingState": binding.State,
		"nodeId": networkState.NodeID, "memberId": binding.MemberID, "privateKeyFile": privateKeyFile,
		"next": "Ask a Plane project admin to approve this binding, then run `looper plane enable " + provider.ID + "`.",
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	printSection(cmd.OutOrStdout(), "Plane Node linked", [][2]any{
		{"providerId", provider.ID}, {"bindingId", binding.ID}, {"bindingState", binding.State},
		{"nodeId", networkState.NodeID}, {"memberId", binding.MemberID}, {"privateKeyFile", privateKeyFile},
		{"next", output["next"]},
	})
	return nil
}

func (r *commandRuntime) planeEnable(cmd *cobra.Command, args []string) error {
	loaded, providerIndex, provider, token, strictBaseURL, err := r.planeCommandContext(args, "")
	if err != nil {
		return err
	}
	if provider.StrictDispatch == nil || strings.TrimSpace(provider.StrictDispatch.BindingID) == "" {
		return errors.New("Plane provider is not linked; run `looper plane link` first")
	}
	path := fmt.Sprintf("/api/workspaces/%s/projects/%s/looper/targets/", url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID))
	var response struct {
		Targets []planeLinkBinding `json:"targets"`
	}
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodGet, path, "", nil, &response); err != nil {
		return fmt.Errorf("check Plane Node binding: %w", err)
	}
	approved := false
	for _, target := range response.Targets {
		if target.ID == provider.StrictDispatch.BindingID && target.NodeID == provider.StrictDispatch.NodeID && target.State == "active" {
			approved = true
			break
		}
	}
	if !approved {
		return errors.New("Plane Node binding is still pending or no longer belongs to this owner")
	}
	strict := *provider.StrictDispatch
	strict.Enabled = true
	if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "enabled": true, "restartRequired": true})
	}
	printSection(cmd.OutOrStdout(), "Plane strict dispatch enabled", [][2]any{{"providerId", provider.ID}, {"nodeId", strict.NodeID}, {"restartRequired", true}, {"next", "Run `looper daemon restart`."}})
	return nil
}

func (r *commandRuntime) planeApprove(cmd *cobra.Command, args []string) error {
	bindingID := strings.TrimSpace(args[0])
	if _, err := parsePlaneProtocolUUID(bindingID); err != nil {
		return fmt.Errorf("binding id: %w", err)
	}
	providerArgs := []string(nil)
	if len(args) == 2 {
		providerArgs = []string{args[1]}
	}
	_, _, provider, token, strictBaseURL, err := r.planeCommandContext(providerArgs, "")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"allowed_roles":       []string{"planner", "worker"},
		"allow_offline_queue": getBoolFlag(cmd, "allow-offline-queue"),
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf(
		"/api/workspaces/%s/projects/%s/looper/bindings/%s/approve/",
		url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID), url.PathEscape(bindingID),
	)
	var binding planeLinkBinding
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPost, path, "application/json", body, &binding); err != nil {
		return fmt.Errorf("approve Plane Node binding: %w", err)
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "bindingId": binding.ID, "state": binding.State})
	}
	printSection(cmd.OutOrStdout(), "Plane Node binding approved", [][2]any{
		{"providerId", provider.ID}, {"bindingId", binding.ID}, {"state", binding.State},
		{"next", "Ask the binding owner to run `looper plane enable " + provider.ID + "`."},
	})
	return nil
}

func (r *commandRuntime) planeSetup(cmd *cobra.Command, args []string) error {
	for index, value := range args[:3] {
		if _, err := parsePlaneProtocolUUID(value); err != nil {
			return fmt.Errorf("role member id %d: %w", index+1, err)
		}
	}
	providerArgs := []string(nil)
	if len(args) == 4 {
		providerArgs = []string{args[3]}
	}
	_, _, provider, token, strictBaseURL, err := r.planeCommandContext(providerArgs, "")
	if err != nil {
		return err
	}
	checklistRevision, err := strconv.Atoi(strings.TrimSpace(getStringFlag(cmd, "checklist-revision")))
	if err != nil || checklistRevision <= 0 {
		return errors.New("--checklist-revision must be a positive signed rollout revision")
	}
	basePath := fmt.Sprintf("/api/workspaces/%s/projects/%s/looper", url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID))
	roleBody, _ := json.Marshal(map[string]any{
		"product_member_id": args[0], "design_member_id": args[1], "qa_member_id": args[2],
	})
	var policyResponse map[string]any
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPut, basePath+"/role-policy/", "application/json", roleBody, &policyResponse); err != nil {
		return fmt.Errorf("configure Plane Looper role policy: %w", err)
	}
	activationBody, _ := json.Marshal(map[string]any{
		"action": "activate", "activation_checklist_revision": checklistRevision,
		"effective_legacy_trigger_label_ids": []string{},
	})
	var integrationResponse map[string]any
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPut, basePath+"/integration/", "application/json", activationBody, &integrationResponse); err != nil {
		return fmt.Errorf("activate Plane Looper integration: %w", err)
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "policy": policyResponse["policy"], "integration": integrationResponse["integration"]})
	}
	printSection(cmd.OutOrStdout(), "Plane Looper project activated", [][2]any{
		{"providerId", provider.ID}, {"productMemberId", args[0]}, {"designMemberId", args[1]}, {"engineeringRule", "dispatch_owner"}, {"qaMemberId", args[2]}, {"checklistRevision", checklistRevision},
		{"next", "Each approved owner runs `looper plane enable " + provider.ID + "` and restarts the daemon."},
	})
	return nil
}

func (r *commandRuntime) planeCommandContext(args []string, strictBaseURLFlag string) (config.LoadedFileConfig, int, config.ProviderConfig, string, string, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
	}
	wanted := ""
	if len(args) == 1 {
		wanted = strings.TrimSpace(args[0])
	}
	index := -1
	for candidateIndex, provider := range loaded.Config.Providers {
		if provider.Kind != config.ProviderKindPlane || (wanted != "" && provider.ID != wanted) {
			continue
		}
		if index != -1 && wanted == "" {
			return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("multiple Plane providers exist; pass a provider id")
		}
		index = candidateIndex
	}
	if index == -1 {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("Plane provider was not found")
	}
	provider := loaded.Config.Providers[index]
	if provider.Workspace == nil || provider.ProjectID == nil || provider.TokenEnv == nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("Plane provider requires workspace, projectId, and tokenEnv")
	}
	token := strings.TrimSpace(os.Getenv(*provider.TokenEnv))
	if token == "" {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", fmt.Errorf("Plane API token environment variable %s is empty", *provider.TokenEnv)
	}
	strictBaseURL := strings.TrimSpace(strictBaseURLFlag)
	if strictBaseURL == "" && provider.StrictDispatch != nil {
		strictBaseURL = strings.TrimSpace(provider.StrictDispatch.BaseURL)
	}
	if strictBaseURL == "" {
		strictBaseURL, err = planeWebOrigin(provider.BaseURL)
		if err != nil {
			return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
		}
	}
	strictBaseURL, err = planeWebOrigin(strictBaseURL)
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
	}
	return loaded, index, provider, token, strings.TrimRight(strictBaseURL, "/"), nil
}

func (r *commandRuntime) planeJSONRequest(ctx context.Context, baseURL, token, method, path, contentType string, body []byte, output any) error {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return err
	}
	return r.planeRawRequest(ctx, parsed.Scheme+"://"+parsed.Host, token, method, parsed.EscapedPath(), contentType, body, output)
}

func (r *commandRuntime) planeRawRequest(ctx context.Context, baseURL, token, method, path, contentType string, body []byte, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", token)
	request.Header.Set("User-Agent", "looper-plane-link/1.0")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	httpClient := *r.httpClient()
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("Plane response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &failure)
		code := strings.TrimSpace(failure.Error)
		detail := strings.TrimSpace(failure.Detail)
		if code != "" && detail != "" {
			return fmt.Errorf("Plane returned HTTP %d (%s): %s", response.StatusCode, code, detail)
		}
		if code != "" {
			return fmt.Errorf("Plane returned HTTP %d (%s)", response.StatusCode, code)
		}
		return fmt.Errorf("Plane returned HTTP %d", response.StatusCode)
	}
	if output != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, output); err != nil {
			return fmt.Errorf("decode Plane response: %w", err)
		}
	}
	return nil
}

func (r *commandRuntime) writePlaneStrictProviderConfig(loaded config.LoadedFileConfig, providerIndex int, strict config.PlaneStrictDispatchConfig) error {
	partial := loaded.Partial
	providers := partial.Providers
	if providers == nil {
		materialized := make([]config.PartialProviderConfig, len(loaded.Config.Providers))
		for index, provider := range loaded.Config.Providers {
			kind := provider.Kind
			materialized[index] = config.PartialProviderConfig{
				ID: provider.ID, Kind: &kind, BaseURL: &provider.BaseURL, GHPath: provider.GHPath,
				Auth: &provider.Auth, TokenEnv: provider.TokenEnv, TeaLogin: provider.TeaLogin, TeaPath: provider.TeaPath,
				Workspace: provider.Workspace, ProjectID: provider.ProjectID, StrictDispatch: provider.StrictDispatch,
			}
		}
		providers = &materialized
	}
	wantedID := loaded.Config.Providers[providerIndex].ID
	found := false
	for index := range *providers {
		if (*providers)[index].ID == wantedID {
			(*providers)[index].StrictDispatch = &strict
			found = true
			break
		}
	}
	if !found {
		return errors.New("Plane provider is not writable in the active config")
	}
	partial.Providers = providers
	raw, err := config.MarshalConfigFile(loaded.Metadata.ConfigPath, partial)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(loaded.Metadata.ConfigPath), 0o755); err != nil {
		return err
	}
	tmp := loaded.Metadata.ConfigPath + ".tmp"
	if err := r.writeFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return renameFile(tmp, loaded.Metadata.ConfigPath)
}

func planeWebOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("Plane URL must be an absolute URL without credentials")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return "", errors.New("Plane URL must use HTTPS outside localhost")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/api/v1")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parsePlaneProtocolUUID(value string) (planeprotocol.UUID, error) {
	var result planeprotocol.UUID
	clean := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	decoded, err := hex.DecodeString(clean)
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("invalid UUID")
	}
	copy(result[:], decoded)
	return result, nil
}

func safeFileComponent(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	if builder.Len() == 0 {
		return "plane"
	}
	return builder.String()
}
