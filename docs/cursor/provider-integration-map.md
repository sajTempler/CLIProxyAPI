I mapped every touch point below, using claude as the reference. **Devin** (commits `f9475276` + `44e62bc8`) is the best template to copy for a vendor with a non-standard wire protocol. **Meta** (`54d4f4c0`) is the smaller template if the Cursor upstream turns out to speak an existing format. All paths are under `/Users/szymon/private/CLIProxyAPI/`.

## 0. Which template to copy

**`f9475276` "add Devin/Cognition provider integration and CLI OAuth" (32 files):**
- `cmd/server/main.go`, `config.example.yaml`
- `internal/api/handlers/management/oauth_sessions.go`
- `internal/auth/devin/{devin_auth.go,devin_auth_test.go,pkce.go}`
- `internal/cmd/{auth_manager.go,devin_login.go}`
- `internal/config/{config.go,config_types.go}`
- `internal/registry/{model_definitions.go,model_definitions_test.go,model_updater.go}`
- `internal/runtime/executor/{devin_executor.go,devin_executor_test.go}`
- `internal/runtime/executor/helps/{devin_models.go,devin_wire.go,cloak_obfuscate.go}` plus their tests
- `internal/signature/provider_compatibility.go`, two interactions translator files (small fixes), `internal/watcher/diff/config_diff.go`
- `sdk/auth/{devin.go,devin_test.go,refresh_registry.go,refresh_registry_test.go}`
- `sdk/cliproxy/auth/conductor_execution.go`, `sdk/cliproxy/{service_auth.go,service_executors.go,service_models.go}`

**`44e62bc8` "implement Devin OAuth flow ... session management" (the management API part):**
- `internal/api/handlers/management/auth_files_devin_oauth.go` (+ test), `oauth_sessions.go`
- `internal/api/{server_management.go,server_routes.go,server_devin_oauth_test.go}`
- `internal/auth/devin/record.go` (+ test), `sdk/auth/devin.go`, `docs/management-devin-oauth.md`

**`54d4f4c0` "add native Meta (Muse Code) provider and OAuth device flow" (32 files):**
- Core pieces: `internal/auth/meta/meta.go`, `internal/cmd/meta_login.go`, `meta_executor.go` (231 lines), `sdk/auth/meta.go`, `registry/models/models.json`, the same `sdk/cliproxy` files, and a `tui/oauth_tab.go` entry added later.
- About half the commit is for a config-file `meta-api-key` variant: `internal/config/*`, `internal/watcher/*`, `sdk/cliproxy/{providers.go,types.go}`. Skip those for a pure OAuth provider.

Kimi (`f5f26f0c`, 14 files) and xAI (`e4c95707`, 24 files) are also small, but older, and the layout has changed since (e.g. `sdk/cliproxy/service.go` was later split into `service_*.go`).

## 1. CLI login flag → saved token file
- **`cmd/server/main.go`:**
  - Flag declared at ~L110/140: `flag.BoolVar(&claudeLogin, "claude-login", ...)`.
  - Must be OR'd into `commandMode` (~L661).
  - Dispatched in the if/else chain (~L740): `} else if claudeLogin { cmd.DoClaudeLogin(cfg, options) }`.
- **`internal/cmd/anthropic_login.go`** `DoClaudeLogin`: builds `sdkAuth.LoginOptions{NoBrowser, CallbackPort, Prompt}` and calls `newAuthManager().Login(ctx, "claude", cfg, opts)`. `internal/cmd/devin_login.go` is a 44-line copy-paste template.
- **`internal/cmd/auth_manager.go`** `newAuthManager()`: add `sdkAuth.NewCursorAuthenticator()` here.
- **Saving:** `sdk/auth/manager.go` `Login` → token store. `sdk/auth/filestore.go` `FileTokenStore.Save` (L75) has two branches:
  - If `auth.Storage != nil`, it calls `Storage.SaveTokenToFile(path)`. This is the claude path, via `ClaudeTokenStorage`.
  - Otherwise it marshals `auth.Metadata` directly. This is the devin path (`internal/auth/devin/record.go` sets `"type": "devin"` in Metadata, with no Storage struct). It is simpler.
- **File format** (`internal/auth/claude/token.go`):
  - Flat JSON with `type:"claude"`, `access_token`, `refresh_token`, `id_token`, `last_refresh`, `expired` (RFC3339), `email`, `account_uuid`, `organization_uuid`, …
  - The file name comes from `claude.CredentialFileName(...)`, i.e. `claude-<email>.json`.
- **Loading:** the `type` field becomes `Auth.Provider` in `filestore.go` L252 and `internal/watcher/synthesizer/file.go` L106. Expiry is read from any of `expired|expire|expires_at|expiresAt|expiry|expires` (`sdk/cliproxy/auth/types.go` `expireKeys`).

## 2. `internal/auth/claude`
- `anthropic_auth.go`:
  - Constants: `AuthURL`, `TokenURL`, `ClientID`, `RedirectURI=http://localhost:54545/callback`, scopes.
  - `ClaudeAuth` methods: `NewClaudeAuth` / `NewClaudeAuthWithProxyURL`, `GenerateAuthURL(state, pkce)`, `ExchangeCodeForTokens`, `RefreshTokens` (deduplicated with singleflight, with backoff), `RefreshTokensWithRetry`, `CreateTokenStorage`, `UpdateTokenStorage`.
- `anthropic.go`: the `PKCECodes`, `ClaudeTokenData` and `ClaudeAuthBundle` types.
- `pkce.go`: `GeneratePKCECodes`.
- `oauth_server.go`: a local callback server (`NewOAuthServer(port)`, `Start`, `WaitForCallback`).
- `token.go`: `ClaudeTokenStorage` with `SetMetadata` and `SaveTokenToFile`.
- `errors.go`, `filename.go`, `html_templates.go`.
- The interactive flow itself is in `sdk/auth/claude.go` `ClaudeAuthenticator.Login`: PKCE, state, local server, browser, a manual-paste fallback after 15 s, code exchange, then it returns `&coreauth.Auth{ID, Provider, FileName, Storage, Metadata}`.
- Cursor's `agent login` uses a polling flow (login URL with uuid + verifier, then poll for the token), which is closer to Meta's device flow: `internal/auth/meta/meta.go` `StartDeviceFlow` / `WaitForAuthorization`.

## 3. Token refresh
- **Declaring:** `sdk/auth/interfaces.go` `Authenticator{Provider(); Login(); RefreshLead() *time.Duration}`. Claude returns `4h`. Devin and Meta return `nil` (no scheduled refresh).
- **Registering:** add one line to `sdk/auth/refresh_registry.go` `init()`: `registerRefreshLead("cursor", func() Authenticator { return NewCursorAuthenticator() })`. That calls `cliproxyauth.RegisterRefreshLeadProvider` (`sdk/cliproxy/auth/types.go` L1122).
- **Worker:** `sdk/cliproxy/auth/conductor_refresh.go` (`StartAutoRefresh`, `shouldRefresh` L117) and `auto_refresh_loop.go`. `shouldRefresh` uses the lead minus expiry, or a `refresh_interval_seconds` value in metadata.
- **The refresh itself is done by the executor:** `ProviderExecutor.Refresh`. For claude this is `internal/runtime/executor/claude_executor_auth.go` L149. It reads `refresh_token` from metadata, calls `RefreshTokensWithRetry`, and writes back `access_token`, `refresh_token`, `expired`, `type` and `last_refresh`. On-demand pre-request fixes can go through the `RequestAuthPreparer` interface (`ShouldPrepareRequestAuth` / `PrepareRequestAuth`).

## 4. Runtime executor
- **Interface:** `sdk/cliproxy/auth/conductor.go` L18 `ProviderExecutor`: `Identifier()`, `Execute`, `ExecuteStream` (returns `*StreamResult`), `Refresh`, `CountTokens`, `HttpRequest`.
- **Optional interfaces:**
  - `requestToFormatResolver.RequestToFormat()` (`conductor_execution.go` L322).
  - `RequestAuthPreparer`, `ExecutionSessionCloser`, `APIKeyConfigExecutor`.
  - `PrepareRequest(*http.Request, *Auth)` by convention.
- **Claude implementation:** `claude_executor.go` (`NewClaudeExecutor`, `Identifier()=="claude"`), plus `_execute.go`, `_stream.go`, `_tokens.go` (upstream count_tokens), `_auth.go`, `_request.go`, and cloaking/compaction extras.
- **Per-request flow:**
  1. Translate with `sdktranslator` from `opts.SourceFormat` to the target format.
  2. `helps.ApplyRequestThinking`.
  3. `helps.ApplyPayloadConfigWithTrackedPaths`. AGENTS.md requires this to be the **last** semantic step on every path.
  4. HTTP call.
  5. `sdktranslator.TranslateNonStream` / `TranslateStream` back to the client format.
  6. `reporter.Publish(usage)`.
- **Simplest model to copy:** Devin's `CountTokens` just returns a length/4 estimate.
- **Registration:** `sdk/cliproxy/service_executors.go`:
  - Add `"cursor"` to the `baselineExecutorAuths()` provider list (~L211).
  - Add `case "cursor": s.coreManager.RegisterExecutor(executor.NewCursorExecutor(cfg))` to `registerExecutorForAuth` (~L311).
- **Selection:** the executor is looked up by `Auth.Provider` equal to `Identifier()`.
- **Target format:** `sdk/cliproxy/auth/conductor_execution.go` `requestToFormat()` has a provider→format switch: claude→`FormatClaude`, devin→`FormatInteractions`, meta/xai→`FormatCodex`. Either add `case "cursor"` there or implement `RequestToFormat` on the executor, as Devin does.
- AGENTS.md also says helper code belongs in `internal/runtime/executor/helps/`, not next to the executors.

## 5. Translators
- **Registration:** in each package's `init.go`, `translator.Register(from=clientFormat, to=upstreamFormat, reqFn, interfaces.TranslateResponse{Stream, NonStream})` (`internal/translator/translator/translator.go` L24). Packages are blank-imported in `internal/translator/init.go`. Directories are named `<upstream>/<client>`.
- **Formats** (`sdk/translator/formats.go`): `openai`, `openai-response`, `claude`, `gemini`, `codex`, `antigravity`, `interactions`.
- **Into the Claude upstream:** from `openai` (chat), `openai-response`, `gemini` and `interactions` (`internal/translator/claude/{openai/chat-completions,openai/responses,gemini,interactions}`). A `claude` client request goes through natively; no translator is needed for that pair.
- **For a non-standard wire format, you need no new translator packages.** Devin shows the pattern:
  - Pick an existing pivot format. Devin's executor returns `FormatInteractions` and translates the client request into it with `sdktranslator.TranslateRequestEnvelope(...)` (`devin_executor.go` ~L399).
  - Convert pivot JSON ↔ the native wire format inside the executor, with helpers in `helps/devin_wire.go` (protobuf).
  - Convert the upstream response back into pivot JSON, then `sdktranslator.TranslateNonStream/TranslateStream(FormatInteractions → client format)`.
  - For Cursor, `FormatClaude` or `FormatOpenAI` is the natural pivot, because every client format already translates to both. You would write only `helps/cursor_wire.go` (+ test).
  - The thinking pipeline (`internal/thinking/provider/*`) is keyed by the target format, so a pivot format reuses the existing applier.
- AGENTS.md forbids standalone edits to `internal/translator/`, which is another reason to use a pivot format.

## 6. Model registry and `/v1/models`
- **Static catalog:** `internal/registry/models/models.json` is embedded. `model_definitions.go` declares `staticModelsJSON{Claude, ..., Devin, Meta}` and exposes `GetClaudeModels()`.
- Three more spots in `model_definitions.go` need a `"cursor"` case:
  - `GetStaticModelDefinitionsByChannel` switch (L511). This also backs v8 `GET /routing/model-definitions/:channel`.
  - `LookupStaticModelInfo` list (L573).
- **Remote refresh:** `model_updater.go` fetches `modelsURLs` (router-for-me/models on GitHub) every 3 h and has per-provider lists in `detectChangedProviders` and `validateModelsCatalog`.
  - **Caveat:** the remote catalog has no `cursor` section, so a refresh would empty a `models.json`-only list.
  - Use Devin's pattern instead: `internal/registry/devin_models.go` `GetDevinModels()` falls back to a Go `staticDevinModels` slice (and has an optional separate catalog updater in `catalog_sources.go`, which you can skip).
- **Exposure:** `sdk/cliproxy/service_models.go` `registerModelsForAuthWithConfigAndCache` switch (~L148 claude, ~L200 devin): `case "cursor": models = registry.GetCursorModels(); models = applyExcludedModels(models, excluded)`.
  - Exclusions come from `cfg.OAuthExcludedModels["cursor"]` (L654).
  - The models are then registered through `GlobalModelRegistry().RegisterClientIfEpochCurrent(authID, provider, models)` (`sdk/cliproxy/model_registry.go` L112).
  - `/v1/models` (`internal/api/server_routes.go` L616 `unifiedModelsHandler`) reads the global registry, so nothing else is needed there.
- **Model aliases:** `sdk/cliproxy/auth/oauth_model_alias.go` `OAuthModelAliasChannel` already defaults to the provider name.

## 7. Management API v8 and TUI
- **Routes:** `internal/api/server_management_v8.go`, prefix `/v8/management`: `GET /oauth/auth-url` → `StartOAuthV8`, plus `/oauth/status`, `DELETE /oauth/session`, `/oauth/callback`, and `/credentials*` (list/upload/delete/refresh/models).
- **Provider dispatch:** `internal/api/handlers/management/auth_files_v8.go` `StartOAuthV8` switches on `?provider=` (`case "claude": h.RequestAnthropicToken(c)`, `case "devin": h.RequestDevinToken(c)`). Add `case "cursor"`.
- **Handlers:**
  - `auth_files_provider_oauth.go`: `RequestAnthropicToken` (L37) for redirect-based PKCE; `RequestMetaToken` (L639) for a device/polling flow, which is the closest match for Cursor. It runs a goroutine that polls, then `h.saveTokenRecord`, then `CompleteOAuthSession(state)`.
  - Devin's version lives in its own file, `auth_files_devin_oauth.go`.
- **Session registry:** `oauth_sessions.go`:
  - `NormalizeOAuthProvider` (L383) must accept `"cursor"`, otherwise the flow is rejected.
  - Call `RegisterOAuthSession(state, "cursor")` when the flow starts.
- **Callbacks:** only needed for redirect flows. Add browser-redirect routes in `internal/api/server_routes.go` (~L150–230: `/anthropic/callback`, `/devin/callback`).
- **Credential listing** (`ListAuthFiles`) is generic and driven by `type`; nothing to add.
- **Web dashboard:** the UI is downloaded from the external repo `router-for-me/Cli-Proxy-API-Management-Center` (`internal/managementasset/updater.go` L30). A provider button there needs a change in that repo.
- **TUI** (`internal/tui/oauth_tab.go`): add to the `oauthProviders` list (L22) and the `apiPath`→`providerKey` switch (~L352).
  - It still calls the deprecated **v0** endpoints: `/v0/management/<x>-auth-url` and `/v0/management/oauth-callback`.
  - For the TUI to work you must also add `mgmt.GET("/cursor-auth-url", ...)` in `internal/api/server_management.go` (~L191–198). That is v0, but it is required; Devin and Meta both did the same.
- **Optional:** `internal/config/config_v8.go` (~L102) maps a provider config section to v8 config paths, e.g. `{"devin", "oauth.providers.devin"}`. Only needed if you add a `cursor:` config block.

## 8. SDK registrations
- `sdk/auth/cursor.go` (new): `CursorAuthenticator` implementing `Provider()`, `RefreshLead()`, `Login()`. Templates are `sdk/auth/claude.go`, `meta.go` and `devin.go`.
- `sdk/auth/refresh_registry.go`: the refresh-lead line from section 3.
- `sdk/cliproxy/service_auth.go` `newDefaultAuthManager()` (L21): add `sdkAuth.NewCursorAuthenticator()`. This duplicates `internal/cmd/auth_manager.go`; both lists must be updated.
- `sdk/cliproxy/service_executors.go`: baseline list and switch case (section 4).
- `sdk/cliproxy/service_models.go`: model case (section 6).
- `sdk/cliproxy/auth/conductor_execution.go` `requestToFormat`: format case (or the executor's `RequestToFormat`).
- **Optional:**
  - `sdk/cliproxy/auth/selector.go` `canonicalLCPProvider` and `home_session_alias.go` `providerFromSourceFormat` (session affinity).
  - `internal/signature/provider_compatibility.go` (cross-provider reasoning signatures).
  - `internal/registry/model_registry.go` `responsesWebSearchProviderPathSupport`.

## 9. Tests to mirror
- **Auth package:** `internal/auth/claude/{anthropic_auth_test.go,token_test.go,filename_test.go,oauth_response_test.go}`, `internal/auth/devin/{devin_auth_test.go,record_test.go}`, `internal/auth/meta/meta_auth_test.go`.
- **SDK auth:**
  - `sdk/auth/{devin_test.go,meta_test.go,xai_test.go}`.
  - `sdk/auth/refresh_registry_test.go` `TestProviderRefreshLeads` has a table of provider → lead and needs a `cursor` row.
- **Executor:** `internal/runtime/executor/{devin_executor_test.go,meta_executor_test.go,xai_executor_test.go}` and `helps/{devin_wire_test.go,devin_models_test.go}`. Per AGENTS.md, add a test that payload config is applied last.
- **Registry:** `internal/registry/{model_definitions_test.go,static_model_lookup_test.go,devin_models_test.go}`.
- **Service:**
  - `sdk/cliproxy/service_excluded_models_test.go`, e.g. `TestRegisterModelsForAuth_MetaOAuthAliasAndExcludedModels`.
  - `sdk/cliproxy/service_xai_executor_binding_test.go` (executor binding).
  - `sdk/cliproxy/auth/oauth_model_alias_test.go`.
- **Management and routes:** `internal/api/handlers/management/auth_files_devin_oauth_test.go`, `internal/api/server_devin_oauth_test.go`, `internal/tui/oauth_tab_test.go`.

**Optional config:** `config.example.yaml` plus a `CursorConfig` in `internal/config/config.go` / `config_types.go` and `internal/watcher/diff/config_diff.go`. These are needed only if Cursor gets its own settings block, as Devin's `sensitive-words` does.