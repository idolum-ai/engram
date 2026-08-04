package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/tmux"
)

const githubBrokerRequestTimeout = githubauth.BrokerExchangeTimeout

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type repeatedInt64Flag []int64

func (f *repeatedInt64Flag) String() string {
	values := make([]string, len(*f))
	for index, value := range *f {
		values[index] = strconv.FormatInt(value, 10)
	}
	return strings.Join(values, ",")
}

func (f *repeatedInt64Flag) Set(value string) error {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("installation ID %q must be a positive integer", value)
	}
	*f = append(*f, parsed)
	return nil
}

func runGitHub(args []string) int {
	if len(args) == 0 {
		printGitHubHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "app":
		return runGitHubApp(args[1:])
	case "exec":
		return runGitHubExec(args[1:])
	case "grant":
		return runGitHubGrant(args[1:])
	case "status":
		return runGitHubStatus(args[1:])
	case "revoke":
		return runGitHubRevoke(args[1:])
	case "help", "--help", "-h":
		printGitHubHelp(os.Stdout)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "github: unknown command:", args[0])
		printGitHubHelp(os.Stderr)
		return 2
	}
}

func runGitHubApp(args []string) int {
	if len(args) == 0 {
		printGitHubAppHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "add":
		return runGitHubAppAdd(args[1:])
	case "list":
		return runGitHubAppList(args[1:])
	case "remove":
		return runGitHubAppRemove(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "github app: unknown command:", args[0])
		printGitHubAppHelp(os.Stderr)
		return 2
	}
}

func runGitHubAppAdd(args []string) int {
	alias, args := leadingAlias(args)
	fs := flag.NewFlagSet("github app add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	appID := fs.Int64("app-id", 0, "GitHub App ID")
	var installationIDs repeatedInt64Flag
	fs.Var(&installationIDs, "installation-id", "GitHub App installation ID; repeatable")
	pemPath := fs.String("pem", "", "path to GitHub App private key PEM")
	telegramUnlock := fs.Bool("telegram-unlock", false, "allow passphrase replies through Telegram's non-E2E bot transport")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if alias == "" && fs.NArg() == 1 {
		alias = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: engram github app add <alias> --app-id ID --installation-id ID [--installation-id ID...] --pem PATH [--telegram-unlock]")
		return 2
	}
	if alias == "" || *appID <= 0 || len(installationIDs) == 0 || strings.TrimSpace(*pemPath) == "" {
		fmt.Fprintln(os.Stderr, "usage: engram github app add <alias> --app-id ID --installation-id ID [--installation-id ID...] --pem PATH [--telegram-unlock]")
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	vaultLock, err := lockfile.Acquire(cfg.LockDir(), lockfile.Key("engram-github-vault"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	defer vaultLock.Close()
	privateKey, err := readPrivateKeyFile(*pemPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	defer githubauth.Zero(privateKey)
	passphrase, err := promptSecret("Passphrase: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	defer githubauth.Zero(passphrase)
	confirmation, err := promptSecret("Confirm passphrase: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	defer githubauth.Zero(confirmation)
	if !bytes.Equal(passphrase, confirmation) {
		fmt.Fprintln(os.Stderr, "github app add: passphrases do not match")
		return 1
	}
	vault, err := githubauth.OpenVault(cfg.GitHubVaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	item, created, err := vault.AddInstallations(alias, *appID, installationIDs, privateKey, passphrase, *telegramUnlock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app add:", err)
		return 1
	}
	action := "updated"
	if created {
		action = "enrolled"
	}
	installationLabels := make([]string, 0, len(item.Installations()))
	for _, installationID := range item.Installations() {
		installationLabels = append(installationLabels, strconv.FormatInt(installationID, 10))
	}
	fmt.Fprintf(os.Stdout, "GitHub App %q %s with installations %s (fingerprint %s).\n",
		item.Alias, action, strings.Join(installationLabels, ","), item.PublicFingerprint)
	if item.TelegramUnlock {
		fmt.Fprintln(os.Stdout, "Warning: Telegram bot chats are not end-to-end encrypted; remote passphrase replies are deleted after processing but traverse Telegram's cloud.")
	}
	return 0
}

func runGitHubAppList(args []string) int {
	fs := flag.NewFlagSet("github app list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: engram github app list [--json]")
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	vault, err := githubauth.OpenVault(cfg.GitHubVaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app list:", err)
		return 1
	}
	apps := vault.List()
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(apps); err != nil {
			fmt.Fprintln(os.Stderr, "github app list:", err)
			return 1
		}
		return 0
	}
	if len(apps) == 0 {
		fmt.Fprintln(os.Stdout, "No GitHub Apps enrolled.")
		return 0
	}
	for _, app := range apps {
		unlock := "local"
		if app.TelegramUnlock {
			unlock = "telegram opt-in"
		}
		installations := make([]string, 0, len(app.Installations()))
		for _, installationID := range app.Installations() {
			installations = append(installations, strconv.FormatInt(installationID, 10))
		}
		fmt.Fprintf(os.Stdout, "%s\tapp=%d\tinstallations=%s\tunlock=%s\tfingerprint=%s\n",
			app.Alias, app.AppID, strings.Join(installations, ","), unlock, app.PublicFingerprint)
	}
	return 0
}

func runGitHubAppRemove(args []string) int {
	alias, args := leadingAlias(args)
	fs := flag.NewFlagSet("github app remove", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	yes := fs.Bool("yes", false, "confirm removal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if alias == "" && fs.NArg() == 1 {
		alias = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: engram github app remove <alias> --yes")
		return 2
	}
	if alias == "" || !*yes {
		fmt.Fprintln(os.Stderr, "usage: engram github app remove <alias> --yes")
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	vaultLock, err := lockfile.Acquire(cfg.LockDir(), lockfile.Key("engram-github-vault"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app remove:", err)
		return 1
	}
	defer vaultLock.Close()
	vault, err := githubauth.OpenVault(cfg.GitHubVaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app remove:", err)
		return 1
	}
	removed, err := vault.Remove(alias)
	if err != nil {
		fmt.Fprintln(os.Stderr, "github app remove:", err)
		return 1
	}
	if !removed {
		fmt.Fprintln(os.Stderr, "github app remove: app is not enrolled")
		return 1
	}
	fmt.Fprintf(os.Stdout, "GitHub App %q removed.\n", alias)
	fmt.Fprintln(os.Stdout, "Existing in-memory leases remain valid until pane revocation, expiry, invalidation, or an Engram restart.")
	return 0
}

func runGitHubExec(args []string) int {
	flagArgs, command, ok := splitCommand(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: engram github exec --app ALIAS [--installation-id ID] --repo OWNER/NAME --permission NAME=read|write -- COMMAND [ARGS...]")
		return 2
	}
	fs := flag.NewFlagSet("github exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	appAlias := fs.String("app", "", "enrolled GitHub App alias")
	installationID := fs.Int64("installation-id", 0, "installation ID; required when the App has more than one")
	localUnlock := fs.Bool("local-unlock", false, "enter the passphrase locally even when Telegram unlock is enabled")
	var repositories repeatedFlag
	var permissions repeatedFlag
	fs.Var(&repositories, "repo", "repository in owner/name form; repeatable")
	fs.Var(&permissions, "permission", "GitHub App permission as name=read|write; repeatable")
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*appAlias) == "" {
		fmt.Fprintln(os.Stderr, "usage: engram github exec --app ALIAS [--installation-id ID] --repo OWNER/NAME --permission NAME=read|write -- COMMAND [ARGS...]")
		return 2
	}
	permissionMap, err := parsePermissionFlags(permissions)
	if err != nil {
		fmt.Fprintln(os.Stderr, "github exec:", err)
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	binding, err := currentGitHubBinding()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github exec:", err)
		return 1
	}
	request := githubauth.BrokerRequest{
		Version:        githubauth.ProtocolVersion,
		Action:         githubauth.ActionExec,
		App:            strings.TrimSpace(*appAlias),
		InstallationID: *installationID,
		Repositories:   repositories,
		Permissions:    permissionMap,
		Command:        command,
		Binding:        binding,
		LocalUnlock:    *localUnlock,
	}
	fmt.Fprintln(os.Stderr, "Requesting GitHub capability from Engram...")
	ctx, cancel := context.WithTimeout(context.Background(), githubBrokerRequestTimeout)
	response, err := requestGitHubCapability(ctx, cfg, request)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github exec:", err)
		return 1
	}
	if strings.TrimSpace(response.Token) == "" {
		fmt.Fprintln(os.Stderr, "github exec: Engram returned no installation token")
		return 1
	}
	return runAuthenticatedChild(command, response.Token)
}

func runGitHubGrant(args []string) int {
	fs := flag.NewFlagSet("github grant", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	appAlias := fs.String("app", "", "enrolled GitHub App alias")
	installationID := fs.Int64("installation-id", 0, "installation ID; required when the App has more than one")
	localUnlock := fs.Bool("local-unlock", false, "enter the passphrase locally even when Telegram unlock is enabled")
	duration := fs.Duration("for", 0, "bounded renewable grant duration")
	purpose := fs.String("purpose", "", "human-readable work-session purpose")
	var repositories repeatedFlag
	var permissions repeatedFlag
	fs.Var(&repositories, "repo", "repository in owner/name form; repeatable")
	fs.Var(&permissions, "permission", "GitHub App permission as name=read|write; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*appAlias) == "" {
		fmt.Fprintln(os.Stderr, "usage: engram github grant --app ALIAS [--installation-id ID] --repo OWNER/NAME --permission NAME=read|write --for DURATION --purpose TEXT")
		return 2
	}
	permissionMap, err := parsePermissionFlags(permissions)
	if err != nil {
		fmt.Fprintln(os.Stderr, "github grant:", err)
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	binding, err := currentGitHubBinding()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github grant:", err)
		return 1
	}
	request := githubauth.BrokerRequest{
		Version:        githubauth.ProtocolVersion,
		Action:         githubauth.ActionGrant,
		App:            strings.TrimSpace(*appAlias),
		InstallationID: *installationID,
		Repositories:   repositories,
		Permissions:    permissionMap,
		Binding:        binding,
		LocalUnlock:    *localUnlock,
		GrantFor:       *duration,
		Purpose:        *purpose,
	}
	request.Normalize()
	if err := request.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "github grant:", err)
		return 2
	}
	fmt.Fprintln(os.Stderr, "Requesting renewable GitHub work-session authority from Engram...")
	ctx, cancel := context.WithTimeout(context.Background(), githubBrokerRequestTimeout)
	response, err := requestGitHubCapability(ctx, cfg, request)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github grant:", err)
		return 1
	}
	if len(response.Grants) != 1 {
		fmt.Fprintln(os.Stderr, "github grant: Engram returned no renewable grant")
		return 1
	}
	fmt.Fprintln(os.Stdout, githubauth.CompactGrantLine(response.Grants[0], time.Now()))
	return 0
}

func requestGitHubCapability(ctx context.Context, cfg config.Config, request githubauth.BrokerRequest) (githubauth.BrokerResponse, error) {
	response, err := githubauth.Request(ctx, cfg.GitHubBrokerSocketPath(), request)
	if err == nil || response.ErrorCode != githubauth.ErrorCodeLocalPassphraseRequired {
		return response, err
	}
	passphrase, promptErr := promptSecret("GitHub App passphrase: ")
	if promptErr != nil {
		return githubauth.BrokerResponse{}, promptErr
	}
	defer githubauth.Zero(passphrase)
	request.Passphrase = passphrase
	return githubauth.Request(ctx, cfg.GitHubBrokerSocketPath(), request)
}

func runGitHubStatus(args []string) int {
	fs := flag.NewFlagSet("github status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	binding, err := currentGitHubBinding()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github status:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	response, err := githubauth.Request(ctx, cfg.GitHubBrokerSocketPath(), githubauth.BrokerRequest{
		Version: githubauth.ProtocolVersion,
		Action:  githubauth.ActionStatus,
		Binding: binding,
	})
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github status:", err)
		return 1
	}
	if *jsonOutput {
		_ = writeGitHubStatusJSON(os.Stdout, response)
		return 0
	}
	if len(response.Grants) == 0 && len(response.Leases) == 0 {
		fmt.Fprintln(os.Stdout, "No active GitHub capability authority for this pane.")
		return 0
	}
	writeGitHubStatus(os.Stdout, response, time.Now())
	return 0
}

func writeGitHubStatusJSON(writer io.Writer, response githubauth.BrokerResponse) error {
	return json.NewEncoder(writer).Encode(struct {
		Grants []githubauth.GrantInfo `json:"grants"`
		Leases []githubauth.LeaseInfo `json:"leases"`
	}{Grants: response.Grants, Leases: response.Leases})
}

func writeGitHubStatus(writer io.Writer, response githubauth.BrokerResponse, now time.Time) {
	for _, grant := range response.Grants {
		fmt.Fprintln(writer, "Work-session grant:", githubauth.CompactGrantLine(grant, now))
		if grant.InstallationID > 0 {
			fmt.Fprintln(writer, "  installation:", grant.InstallationID)
		}
		fmt.Fprintln(writer, "  purpose:", grant.Purpose)
		fmt.Fprintln(writer, "  expires:", grant.ExpiresAt.Local().Format(time.RFC3339))
		for _, repository := range grant.Repositories {
			fmt.Fprintln(writer, "  repo ceiling:", repository)
		}
		names := make([]string, 0, len(grant.Permissions))
		for name := range grant.Permissions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(writer, "  permission ceiling: %s=%s\n", name, grant.Permissions[name])
		}
	}
	for _, lease := range response.Leases {
		fmt.Fprintln(writer, "Current token lease:", githubauth.CompactLeaseLine(lease, now))
		if lease.InstallationID > 0 {
			fmt.Fprintln(writer, "  installation:", lease.InstallationID)
		}
		for _, repository := range lease.Repositories {
			fmt.Fprintln(writer, "  repo:", repository)
		}
		names := make([]string, 0, len(lease.Permissions))
		for name := range lease.Permissions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(writer, "  permission: %s=%s\n", name, lease.Permissions[name])
		}
	}
}

func runGitHubRevoke(args []string) int {
	fs := flag.NewFlagSet("github revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadGitHubConfig(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	binding, err := currentGitHubBinding()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github revoke:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err = githubauth.Request(ctx, cfg.GitHubBrokerSocketPath(), githubauth.BrokerRequest{
		Version: githubauth.ProtocolVersion,
		Action:  githubauth.ActionRevoke,
		Binding: binding,
	})
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "github revoke:", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "GitHub capability grant and token lease revoked for this pane.")
	return 0
}

func currentGitHubBinding() (githubauth.Binding, error) {
	paneID := strings.TrimSpace(os.Getenv("TMUX_PANE"))
	if paneID == "" {
		return githubauth.Binding{}, fmt.Errorf("TMUX_PANE is not set; run this command inside tmux")
	}
	manager := tmux.New(tmux.ExecRunner{})
	ctx, cancel := tmux.TimeoutContext(context.Background())
	defer cancel()
	pane, err := manager.InspectPane(ctx, paneID)
	if err != nil {
		return githubauth.Binding{}, fmt.Errorf("inspect requesting tmux pane: %w", err)
	}
	serverID, err := manager.CurrentServerID(ctx)
	if err != nil {
		return githubauth.Binding{}, fmt.Errorf("read Engram tmux identity: %w", err)
	}
	return githubauth.Binding{ServerID: serverID, WindowID: pane.WindowID, PaneID: pane.ID}, nil
}

func loadGitHubConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	if err := config.EnsureDirs(cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func parsePermissionFlags(values []string) (map[string]string, error) {
	permissions := make(map[string]string, len(values))
	for _, value := range values {
		name, level, found := strings.Cut(value, "=")
		if !found || strings.TrimSpace(name) == "" || strings.TrimSpace(level) == "" {
			return nil, fmt.Errorf("permission %q must use name=read or name=write", value)
		}
		name = strings.TrimSpace(name)
		level = strings.ToLower(strings.TrimSpace(level))
		if existing, duplicate := permissions[name]; duplicate && existing != level {
			return nil, fmt.Errorf("permission %q was requested at conflicting levels", name)
		}
		permissions[name] = level
	}
	return permissions, nil
}

func readPrivateKeyFile(path string) ([]byte, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open private key: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat private key: %w", err)
	}
	stat, ownerOK := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownerOK || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("private key must be a private regular file owned by uid %d", os.Geteuid())
	}
	data, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	if len(data) > 128<<10 {
		return nil, fmt.Errorf("private key exceeds 131072 bytes")
	}
	return data, nil
}

func promptSecret(prompt string) ([]byte, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open controlling terminal for passphrase: %w", err)
	}
	defer tty.Close()
	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return nil, err
	}
	secret, err := readPassword(tty)
	_, _ = fmt.Fprintln(tty)
	if err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	return secret, nil
}

func readSecretLine(file *os.File) ([]byte, error) {
	const maxPassphraseBytes = 4096
	reader := bufio.NewReader(io.LimitReader(file, maxPassphraseBytes+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(line) > maxPassphraseBytes {
		return nil, fmt.Errorf("passphrase exceeds %d bytes", maxPassphraseBytes)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line, nil
}

func runAuthenticatedChild(command []string, token string) int {
	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = githubChildEnvironment(os.Environ(), token)
	err := child.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "github exec:", err)
	return 1
}

func githubChildEnvironment(parent []string, token string) []string {
	environment := make([]string, 0, len(parent)+1)
	for _, variable := range parent {
		if strings.HasPrefix(variable, "GH_TOKEN=") || strings.HasPrefix(variable, "GITHUB_TOKEN=") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, "GH_TOKEN="+token)
}

func splitCommand(args []string) ([]string, []string, bool) {
	for index, argument := range args {
		if argument == "--" {
			if index+1 >= len(args) {
				return nil, nil, false
			}
			return args[:index], args[index+1:], true
		}
	}
	return nil, nil, false
}

func leadingAlias(args []string) (string, []string) {
	if len(args) != 0 && !strings.HasPrefix(args[0], "-") {
		return strings.TrimSpace(args[0]), args[1:]
	}
	return "", args
}

func printGitHubHelp(output io.Writer) {
	fmt.Fprint(output, `Usage:
  engram github app add <alias> --app-id ID --installation-id ID [--installation-id ID...] --pem PATH [--telegram-unlock]
  engram github app list [--json]
  engram github app remove <alias> --yes
  engram github grant --app ALIAS [--installation-id ID] --repo OWNER/NAME --permission NAME=read|write --for DURATION --purpose TEXT
  engram github exec --app ALIAS [--installation-id ID] --repo OWNER/NAME --permission NAME=read|write -- COMMAND [ARGS...]
  engram github status [--json]
  engram github revoke
`)
}

func printGitHubAppHelp(output io.Writer) {
	fmt.Fprint(output, `Usage:
  engram github app add <alias> --app-id ID --installation-id ID [--installation-id ID...] --pem PATH [--telegram-unlock]
  engram github app list [--json]
  engram github app remove <alias> --yes
`)
}
