package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/admin"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/oauth"
	"quickrelay-studio/internal/proxy"
	"quickrelay-studio/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return runServer(nil)
	}
	switch args[0] {
	case "server":
		return runServer(args[1:])
	case "login":
		return login(args[1:])
	case "accounts", "list":
		return listAccounts(args[1:])
	case "refresh":
		return refreshAccounts(args[1:])
	case "logout":
		return logout(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		if strings.HasPrefix(args[0], "-") {
			return runServer(args)
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quickrelay - QuickRelay Studio (OpenAI & Anthropic Dual-Protocol Gateway)

Usage:
  quickrelay server   [-config config.json] [-addr 127.0.0.1:8080]
  quickrelay login    [-config config.json] [-provider social|enterprise|gateway|social_gateway]
                      [-stage prod|beta|integ|dev] [-region REGION] [-port 18080]
                      [-timeout 1h] [-open] [-manual]
  quickrelay accounts [-config config.json]
  quickrelay refresh  [-config config.json] [-account ID]
  quickrelay logout   [-config config.json] (-all | -account ID)

Running without a subcommand is equivalent to "server".

config.json is created with defaults on first use. Accounts are stored in the
SQLite database named by "database" in config.json. When multiple accounts are
present, requests rotate through them round-robin.
`)
}

func loadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

func openStore(cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", cfg.Database, err)
	}
	return st, nil
}

func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	addr := fs.String("addr", "", "override the HTTP listen address without changing config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	mgr := accounts.New(st, cfg)
	count, _ := mgr.EnabledCount(context.Background())
	handler := proxy.NewServer(cfg, mgr).Handler()
	if cfg.WebUI.Enabled {
		panel, err := admin.NewServer(cfg, st, mgr, *addr != "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "webui:", err)
			return 1
		}
		defer panel.Close()
		handler = panel.Handler(handler)
		log.Printf("WebUI enabled at / (management authentication is independent of proxy API keys)")
		if !panel.Configured() {
			log.Printf("WebUI login is disabled until the admin password environment variable is configured")
		}
	}
	listenAddr := cfg.Addr
	if *addr != "" {
		listenAddr = *addr
	}
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("quickrelay-studio listening on %s (config=%s db=%s accounts=%d auth=%v)",
		listenAddr, cfg.Path, cfg.Database, count, len(cfg.APIKeys) > 0)
	log.Printf("models=%s default_model=%s request_timeout=%s refresh_before=%s max_attempts=%d keep_sessions=%v session_reuse=%v session_ttl=%s",
		strings.Join(cfg.Models, ","), cfg.DefaultModel, cfg.RequestTimeout.Duration(),
		cfg.RefreshBefore.Duration(), cfg.MaxAttempts, cfg.KeepSessions, cfg.SessionReuse, cfg.SessionTTL.Duration())
	if count == 0 {
		log.Printf("no accounts stored; run `quickrelay login` to add one")
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stopUsageSync := mgr.StartUsageSync(stop)
	defer stopUsageSync()
	listenErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			cancel()
		}
	}()

	<-stop.Done()
	log.Printf("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	select {
	case err := <-listenErr:
		log.Printf("listen: %v", err)
		return 1
	default:
		return 0
	}
}

func login(args []string) int {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	provider := fs.String("provider", "", "social, enterprise, gateway or social_gateway (default from config)")
	stage := fs.String("stage", "", "prod, beta, integ or dev (default from config)")
	region := fs.String("region", "", "IDC region (social) or enterprise discovery region prefix")
	port := fs.Int("port", 0, "localhost callback port (default from config)")
	timeout := fs.Duration("timeout", 0, "wait duration for the browser callback (default from config)")
	open := fs.Bool("open", false, "open the login URL in the default browser")
	manual := fs.Bool("manual", false, "paste browser callback URLs manually (for servers without a reachable localhost)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	if *provider == "" {
		*provider = cfg.OAuth.Provider
	}
	if *stage == "" {
		*stage = cfg.OAuth.BuildStage
	}
	if *port == 0 {
		*port = cfg.OAuth.CallbackPort
	}
	if *timeout == 0 {
		*timeout = cfg.OAuth.Timeout.Duration()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := log.New(os.Stdout, "[login] ", 0)
	opts := oauth.LoginOptions{
		Provider:    *provider,
		Stage:       *stage,
		Region:      *region,
		Port:        *port,
		Timeout:     *timeout,
		AppVersion:  cfg.AppVersion,
		OpenBrowser: *open,
		Logf:        logger.Printf,
	}
	var acct *store.Account
	if *manual {
		acct, err = oauth.LoginManual(ctx, opts, st, os.Stdin, os.Stdout)
	} else {
		acct, err = oauth.Login(ctx, opts, st)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[login] failed:", err)
		return 1
	}
	fmt.Printf("account %d ready: %s (%s, %s)\n", acct.ID, displayAccount(acct), acct.Provider, acct.WSHost)
	return 0
}

func listAccounts(args []string) int {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	list, err := st.List(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "list accounts:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tPROVIDER\tSTAGE\tREGION\tEXPIRES\tCALLS\tOK/FAILED\tELAPSED\tTOKENS\tUSD\tCREDITS\tSELECTS\tSTATUS")
	now := time.Now()
	for i := range list {
		a := &list[i]
		status := "ready"
		if a.Disabled {
			status = "disabled"
		} else if a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix() {
			status = "expired"
		}
		if a.LastError != "" {
			status += " (" + a.LastError + ")"
		}
		cost := "unknown"
		if a.Usage.CostUSD != nil {
			cost = fmt.Sprintf("%.6g (%d/%d)", *a.Usage.CostUSD, a.Usage.CostReportedCalls, a.Usage.CallCount)
		}
		credits := make([]string, 0, len(a.Usage.Credits))
		for _, credit := range a.Usage.Credits {
			credits = append(credits, fmt.Sprintf("%.6g %s (%d/%d)", credit.Amount, credit.Unit, credit.ReportedCalls, a.Usage.CallCount))
		}
		creditSummary := strings.Join(credits, "; ")
		if creditSummary == "" {
			creditSummary = "unknown"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d/%d\t%s\t%d\t%s\t%s\t%d\t%s\n",
			a.ID, displayAccount(a), a.Provider, a.BuildStage, a.Region,
			formatEpoch(a.ExpiresAt), a.Usage.CallCount, a.Usage.SuccessCount, a.Usage.FailureCount,
			(time.Duration(a.Usage.TotalDurationMS) * time.Millisecond).Round(time.Millisecond),
			a.Usage.TotalTokens, cost, creditSummary, a.UseCount, status)
	}
	_ = tw.Flush()
	return 0
}

func refreshAccounts(args []string) int {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "refresh only this account ID (default: all)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	mgr := accounts.New(st, cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *accountID > 0 {
		a, err := mgr.RefreshOne(ctx, *accountID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "account %d: %v\n", *accountID, err)
			return 1
		}
		fmt.Printf("account %d refreshed: %s expires %s\n", a.ID, displayAccount(a), formatEpoch(a.ExpiresAt))
		return 0
	}

	results, err := mgr.RefreshAll(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	failed := 0
	for _, res := range results {
		if res.Err != nil {
			failed++
			id := int64(0)
			if res.Account != nil {
				id = res.Account.ID
			}
			fmt.Fprintf(os.Stderr, "account %d: %v\n", id, res.Err)
			continue
		}
		fmt.Printf("account %d refreshed: %s expires %s\n",
			res.Account.ID, displayAccount(res.Account), formatEpoch(res.Account.ExpiresAt))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func logout(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "delete only this account ID")
	all := fs.Bool("all", false, "delete every stored account")
	_ = fs.Parse(args)

	if *accountID <= 0 && !*all {
		fmt.Fprintln(os.Stderr, "logout requires -all or -account ID")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	ctx := context.Background()
	if *all {
		n, err := st.DeleteAll(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "logout:", err)
			return 1
		}
		fmt.Printf("removed %d account(s)\n", n)
		return 0
	}
	ok, err := st.Delete(ctx, *accountID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "account %d not found\n", *accountID)
		return 1
	}
	fmt.Printf("removed account %d\n", *accountID)
	return 0
}

func displayAccount(a *store.Account) string {
	for _, v := range []string{a.Email, a.AccountName, a.AWSAccountID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "-"
}

func formatEpoch(v int64) string {
	if v <= 0 {
		return "unknown"
	}
	return time.Unix(v, 0).Local().Format("2006-01-02 15:04")
}
