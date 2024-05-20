//go:generate go run ./frontend/generate.go main ./frontend ./frontend_gen.go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/docker/go-units"
	"github.com/heetch/confita"
	"github.com/heetch/confita/backend/env"
	"github.com/heetch/confita/backend/flags"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/puzpuzpuz/xsync/v3"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Host        string `config:"host"`
	Port        uint32 `config:"port"`
	LogLevel    string `config:"log_level"`
	DataPath    string `config:"data_path"`
	RemoteHost  string `config:"remote_host"`
	RemotePort  uint32 `config:"remote_port"`
	RemoteUser  string `config:"remote_user"`
	RsyncSSHKey string `config:"rsync_ssh_key"`
	LsSSHKey    string `config:"ls_ssh_key"`
	KnownHosts  string `config:"known_hosts"`
}

type Dir struct {
	Path     string
	Name     string
	Children map[string]*Dir
	Parent   *Dir
	Synced   bool
}

type Sync struct {
	Path       string
	Progress   uint
	Speed      uint
	Downloaded uint
	TimeLeft   string
	Context    context.Context
	Cancel     context.CancelFunc
}

type Result[T any] struct {
	Results []T    `json:"results,omitempty"`
	Error   string `json:"error,omitempty"`
}

type DirResult struct {
	Path   string `json:"path"`
	Synced bool   `json:"synced"`
}

type SyncResult struct {
	Path       string `json:"path"`
	Progress   uint   `json:"progress"`
	Speed      uint   `json:"speed"`
	Downloaded uint   `json:"downloaded"`
	TimeLeft   string `json:"time_left"`
}

type SyncRequest struct {
	Path string `json:"path"`
}

type CancelSyncRequest SyncRequest

func buildLocalTree(config Config) (map[string]*Dir, error) {
	pathMap := map[string]*Dir{}

	dir, err := filepath.Abs(config.DataPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk dir item: %w", err)
		}
		if d.IsDir() {
			path = strings.TrimPrefix(path, dir)

			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}

			parent := pathMap[filepath.Dir(path)]
			item := Dir{
				Path:     path,
				Name:     d.Name(),
				Children: map[string]*Dir{},
				Parent:   parent,
				Synced:   false,
			}
			if parent != nil {
				parent.Children[item.Name] = &item
			}
			pathMap[item.Path] = &item
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk dir: %w", err)
	}
	return pathMap, nil
}

func buildRemoteTree(logger *slog.Logger, ctx context.Context, config Config, localPathMap map[string]*Dir) (map[string]*Dir, error) {
	pathMap := map[string]*Dir{}

	cmd := exec.CommandContext(ctx, "ssh", "-T", "-p", fmt.Sprintf("%d", config.RemotePort), "-o", fmt.Sprintf("UserKnownHostsFile=%s", config.KnownHosts), "-o", "StrictHostKeyChecking=yes", "-o", "PasswordAuthentication=no", "-i", config.LsSSHKey, fmt.Sprintf("%s@%s", config.RemoteUser, config.RemoteHost))

	logger.Debug("ls cmd", slog.Any("args", cmd.Args))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("get stdout pipe: %w", err)
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logger.Info(scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	err = cmd.Start()

	if err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	for scanner.Scan() {
		path := scanner.Text()

		parent := pathMap[filepath.Dir(path)]
		item := Dir{
			Path:     path,
			Name:     filepath.Base(path),
			Children: map[string]*Dir{},
			Parent:   parent,
			Synced:   true,
		}
		if parent != nil {
			parent.Children[item.Name] = &item
		}
		pathMap[item.Path] = &item
	}

	err = cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("wait command: %w", err)
	}

	var setNotSynced func(*Dir)
	setNotSynced = func(item *Dir) {
		item.Synced = false
		if item.Parent != nil {
			setNotSynced(item.Parent)
		}
	}

	for path, item := range pathMap {
		if _, ok := localPathMap[path]; !ok {
			setNotSynced(item)
		}
	}

	return pathMap, nil
}

func startSync(logger *slog.Logger, ctx context.Context, config Config, runningSyncs *xsync.MapOf[string, *Sync], currentSync *Sync) {
	defer func() {
		runningSyncs.Delete(currentSync.Path)
	}()

	ctx, cancel := context.WithCancel(ctx)

	syncPath, _ := filepath.Split(filepath.Join(config.DataPath, currentSync.Path))
	err := os.MkdirAll(syncPath, 0755)
	if err != nil {
		logger.Error("create path failed", slog.String("error", err.Error()))
		cancel()
		return
	}

	cmd := exec.CommandContext(ctx, "rsync", "-a", "--info=progress2", "-e", fmt.Sprintf("ssh -i %s -p %d -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o PasswordAuthentication=no", config.RsyncSSHKey, config.RemotePort, config.KnownHosts), fmt.Sprintf("%s@%s:%s", config.RemoteUser, config.RemoteHost, filepath.Join(currentSync.Path)), syncPath)

	logger.Debug("rsync cmd", slog.Any("args", cmd.Args))

	go func() {
		<-currentSync.Context.Done()

		if cmd.Process.Pid != -1 {
			err := cmd.Process.Signal(syscall.SIGTERM)
			if err != nil {
				logger.Error("terminate err", slog.String("error", err.Error()))
			}
			time.Sleep(time.Second * 5)
			cancel()
		}
	}()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logger.Error("stderr pipe", slog.String("error", err.Error()))
		return
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logger.Info(scanner.Text())
		}
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Error("get stdout pipe", slog.String("error", err.Error()))
		return
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Split(bufio.ScanWords)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasSuffix(text, "%") {
				v, err := strconv.Atoi(strings.TrimSuffix(text, "%"))
				if err != nil {
					logger.Error("failed parse string", slog.String("value", text), slog.String("error", err.Error()))
				} else {
					currentSync.Progress = uint(v)
				}
				continue
			}
			if strings.HasSuffix(text, "/s") {
				v, err := units.FromHumanSize(strings.TrimSuffix(text, "/s"))
				if err != nil {
					logger.Error("failed parse string", slog.String("value", text), slog.String("error", err.Error()))
				} else {
					currentSync.Speed = uint(v)
				}
				continue
			}
			if strings.Contains(text, ":") {
				currentSync.TimeLeft = text
				continue
			}
			v, err := strconv.Atoi(strings.ReplaceAll(text, ",", ""))
			if err != nil {
				logger.Error("failed parse string", slog.String("value", text), slog.String("error", err.Error()))
			} else {
				currentSync.Downloaded = uint(v)
			}
		}
	}()

	err = cmd.Run()
	if err != nil {
		logger.Error("wait command", slog.String("error", err.Error()))
	}
}

func sync(logger *slog.Logger, ctx context.Context, config Config, runningSyncs *xsync.MapOf[string, *Sync], path string) bool {
	ctx, cancel := context.WithCancel(ctx)

	newSync := &Sync{
		Path:     path,
		Progress: 0,
		Speed:    0,
		Context:  ctx,
		Cancel:   cancel,
	}

	if _, ok := runningSyncs.LoadOrStore(path, newSync); ok {
		return false
	}

	go startSync(logger, ctx, config, runningSyncs, newSync)

	return true
}

func ListDirs(logger *slog.Logger, ctx context.Context, config Config) echo.HandlerFunc {
	return func(c echo.Context) error {
		localPathMap, err := buildLocalTree(config)
		if err != nil {
			return fmt.Errorf("list local: %w", err)
		}
		pathMap, err := buildRemoteTree(logger, ctx, config, localPathMap)
		if err != nil {
			return fmt.Errorf("list remote: %w", err)
		}

		keys := make([]string, 0, len(pathMap))

		for k := range pathMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		result := Result[DirResult]{
			Error:   "",
			Results: make([]DirResult, 0),
		}

		for _, k := range keys {
			result.Results = append(result.Results, DirResult{Path: pathMap[k].Path, Synced: pathMap[k].Synced})
		}

		return c.JSON(http.StatusOK, result)
	}
}

func ListSyncs(runningSyncs *xsync.MapOf[string, *Sync]) echo.HandlerFunc {
	return func(c echo.Context) error {
		result := Result[SyncResult]{
			Error:   "",
			Results: make([]SyncResult, 0),
		}

		runningSyncs.Range(func(key string, value *Sync) bool {
			result.Results = append(result.Results, SyncResult{
				Path:       value.Path,
				Progress:   value.Progress,
				Speed:      value.Speed,
				Downloaded: value.Downloaded,
				TimeLeft:   value.TimeLeft,
			})
			return true
		})

		return c.JSON(http.StatusOK, result)
	}
}

func StartSync(logger *slog.Logger, ctx context.Context, config Config, runningSyncs *xsync.MapOf[string, *Sync]) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := &SyncRequest{}

		err := c.Bind(request)
		if err != nil {
			return fmt.Errorf("load request: %w", err)
		}

		if sync(logger, ctx, config, runningSyncs, request.Path) {
			return c.JSON(http.StatusOK, Result[string]{})
		}
		return c.JSON(http.StatusConflict, Result[string]{Error: "sync already started"})
	}
}

func CancelSync(runningSyncs *xsync.MapOf[string, *Sync]) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := &CancelSyncRequest{}

		err := c.Bind(request)
		if err != nil {
			return fmt.Errorf("load request: %w", err)
		}

		if currentSync, ok := runningSyncs.Load(request.Path); ok {
			currentSync.Cancel()
		}
		return c.JSON(http.StatusOK, Result[string]{})
	}
}

func customHTTPErrorHandler(err error, c echo.Context) {
	code := http.StatusInternalServerError
	var he *echo.HTTPError
	if errors.As(err, &he) {
		code = he.Code
	}
	if err := c.JSON(code, Result[string]{
		Error: err.Error(),
	}); err != nil {
		c.Logger().Error(err)
	}
}

func testConfig(config Config) error {
	var errs []error
	if config.DataPath == "" {
		errs = append(errs, errors.New("data path must be specified"))
	}
	if config.RemoteUser == "" {
		errs = append(errs, errors.New("remote user must be specified"))
	}
	if config.RemotePort == 0 {
		errs = append(errs, errors.New("remote port must be specified"))
	}
	if config.RemoteUser == "" {
		errs = append(errs, errors.New("remote user must be specified"))
	}
	if config.RsyncSSHKey == "" {
		errs = append(errs, errors.New("rsync ssh key file must be specified"))
	}
	if config.LsSSHKey == "" {
		errs = append(errs, errors.New("ls ssh key file must be specified"))
	}
	if config.KnownHosts == "" {
		errs = append(errs, errors.New("known host file  must be specified"))
	}

	return errors.Join(errs...)
}

func main() {
	quit, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	loader := confita.NewLoader(
		env.NewBackend(),
		flags.NewBackend(),
	)

	config := Config{
		Host:     "",
		Port:     8080,
		LogLevel: "Info",
	}

	err := loader.Load(context.Background(), &config)
	if err != nil {
		log.Fatalln("failed to load config", err)
	}

	runningSyncs := xsync.NewMapOf[string, *Sync]()

	var level slog.Level
	err = level.UnmarshalText([]byte(config.LogLevel))
	if err != nil {
		log.Fatalln(err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))

	err = testConfig(config)
	if err != nil {
		logger.Error("config error", slog.String("error", strings.ReplaceAll(err.Error(), "\n", " | ")))
		os.Exit(1)
	}

	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:   true,
		LogURI:      true,
		LogError:    true,
		HandleError: false, // forwards error to the global error handler, so it can decide appropriate status code
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if v.Error == nil {
				logger.LogAttrs(context.Background(), slog.LevelInfo, "REQUEST",
					slog.String("uri", v.URI),
					slog.Int("status", v.Status),
				)
			} else {
				logger.LogAttrs(context.Background(), slog.LevelError, "REQUEST_ERROR",
					slog.String("uri", v.URI),
					slog.Int("status", v.Status),
					slog.String("err", v.Error.Error()),
				)
			}
			return nil
		},
	}))
	e.HTTPErrorHandler = customHTTPErrorHandler

	e.StaticFS("/", echo.MustSubFS(content, "frontend/build"))
	e.GET("/api/dirs", ListDirs(logger, quit, config))
	e.GET("/api/syncs", ListSyncs(runningSyncs))
	e.POST("/api/sync", StartSync(logger, quit, config, runningSyncs))
	e.POST("/api/cancel", CancelSync(runningSyncs))

	s := http.Server{
		Addr:    fmt.Sprintf("%s:%d", config.Host, config.Port),
		Handler: e,
	}
	go func(srv *http.Server) {
		logger.Info("listen", slog.String("host", config.Host), slog.Uint64("port", uint64(config.Port)))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server listen error", slog.String("error", err.Error()))
			os.Exit(1)
		}
		cancel() // in case server returns before ctrl+c
	}(&s)

	// Wait until interrupt signal to start shutdown
	<-quit.Done()

	// start gracefully shutdown with a timeout of 10 seconds.
	ctx, cancelGC := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelGC()

	if err := s.Shutdown(ctx); err != nil {
		logger.Error("server shutdown", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
