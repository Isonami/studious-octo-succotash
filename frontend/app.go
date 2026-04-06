package frontend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maxence-charriere/go-app/v10/pkg/app"
)

var registerOnce sync.Once

type dir struct {
	Path   string `json:"path"`
	Synced bool   `json:"synced"`
}

type syncItem struct {
	Path       string `json:"path"`
	Progress   uint   `json:"progress"`
	Speed      uint   `json:"speed"`
	Downloaded uint   `json:"downloaded"`
	TimeLeft   string `json:"time_left"`
}

type baseResponse struct {
	Error string `json:"error"`
}

type dirsResponse struct {
	baseResponse
	Results []dir `json:"results"`
}

type syncsResponse struct {
	baseResponse
	Results []syncItem `json:"results"`
}

type pathRequest struct {
	Path string `json:"path"`
}

type dashboard struct {
	app.Compo

	dirs   []dir
	syncs  []syncItem
	errors []string
	active bool
}

func RegisterRoutes() {
	registerOnce.Do(func() {
		app.Route("/", app.NewZeroComponentFactory(&dashboard{}))
	})
}

func NewHandler() *app.Handler {
	RegisterRoutes()
	return &app.Handler{
		Name: "Syncer",
		Icon: app.Icon{
			Default: "/web/favicon.png",
			SVG:     "/web/favicon.svg",
		},
		ShortName:       "Syncer",
		Title:           "Syncer",
		Description:     "Remote sync dashboard.",
		BackgroundColor: "#0f172a",
		ThemeColor:      "#0f172a",
		LoadingLabel:    "Loading Syncer {progress}%",
		Styles:          []string{"/web/main.css"},
		CacheableResources: []string{
			"/web/main.css",
			"/web/app.wasm",
		},
	}
}

func (d *dashboard) OnMount(ctx app.Context) {
	d.active = true
	d.refreshAll(ctx)
	d.schedulePoll(ctx)
}

func (d *dashboard) OnDismount() {
	d.active = false
}

func (d *dashboard) Render() app.UI {
	return app.Div().Class("app-shell").Body(
		app.Header().Class("topbar").Body(
			app.Div().Class("topbar-inner").Body(
				app.Div().Class("brand-block").Body(
					app.P().Class("eyebrow").Text(""),
					app.H1().Class("brand").Text("Syncer"),
					app.P().Class("subtitle").Text("Remote directories and active syncs."),
				),
				app.Div().Class("status-chip").Body(
					app.Span().Class("status-label").Text("Active syncs"),
					app.Strong().Text(fmt.Sprintf("%d", len(d.syncs))),
				),
			),
		),
		app.Div().Class("page").Body(
			d.renderErrors(),
			app.Div().Class("cards").Body(
				app.Section().Class("card").Body(
					app.Div().Class("card-head").Body(
						app.H2().Text("Syncs"),
						app.P().Text("Refreshing every 2 seconds."),
					),
					d.renderSyncsTable(),
				),
				app.Section().Class("card").Body(
					app.Div().Class("card-head").Body(
						app.H2().Text("Remote dirs"),
						app.P().Text("Start, re-run, or remove synced directories."),
					),
					d.renderDirsTable(),
				),
			),
		),
	)
}

func (d *dashboard) renderErrors() app.UI {
	if len(d.errors) == 0 {
		return app.Div()
	}

	items := make([]app.UI, 0, len(d.errors))
	for i, message := range d.errors {
		index := i
		items = append(items, app.Div().Class("notice error").Body(
			app.Div().Class("notice-copy").Text(message),
			app.Button().
				Class("action-button secondary compact").
				Type("button").
				Text("Dismiss").
				OnClick(func(ctx app.Context, e app.Event) {
					d.dismissError(index)
					ctx.Update()
				}),
		))
	}

	return app.Div().Class("notice-stack").Body(items...)
}

func (d *dashboard) renderSyncsTable() app.UI {
	rows := make([]app.UI, 0, len(d.syncs)+1)
	if len(d.syncs) == 0 {
		rows = append(rows, app.Tr().Body(
			app.Td().ColSpan(6).Class("empty-state").Text("No syncs are running right now."),
		))
	} else {
		for _, sync := range d.syncs {
			current := sync
			rows = append(rows, app.Tr().Body(
				app.Td().Class("path-cell").Text(current.Path),
				app.Td().Body(
					app.Div().Class("progress-wrap").Body(
						app.Div().Class("progress-track").Body(
							app.Div().
								Class("progress-bar").
								Style("width", fmt.Sprintf("%d%%", current.Progress)),
						),
						app.Span().Class("progress-label").Text(fmt.Sprintf("%d%%", current.Progress)),
					),
				),
				app.Td().Text(emptyDash(current.TimeLeft)),
				app.Td().Text(formatIEC(uint64(current.Downloaded))),
				app.Td().Text(formatIEC(uint64(current.Speed))+"/s"),
				app.Td().Class("actions-cell").Body(
					app.Button().
						Class("action-button danger").
						Type("button").
						Text("Cancel").
						OnClick(func(ctx app.Context, e app.Event) {
							d.handleCancel(ctx, current.Path)
						}),
				),
			))
		}
	}

	return app.Div().Class("table-wrap").Body(
		app.Table().Class("data-table").Body(
			app.THead().Body(
				app.Tr().Body(
					app.Th().Text("Path"),
					app.Th().Text("Progress"),
					app.Th().Text("Time Left"),
					app.Th().Text("Transferred"),
					app.Th().Text("Speed"),
					app.Th().Text(""),
				),
			),
			app.TBody().Body(rows...),
		),
	)
}

func (d *dashboard) renderDirsTable() app.UI {
	rows := make([]app.UI, 0, len(d.dirs)+1)
	if len(d.dirs) == 0 {
		rows = append(rows, app.Tr().Body(
			app.Td().ColSpan(3).Class("empty-state").Text("No remote directories found."),
		))
	} else {
		for _, dir := range d.dirs {
			current := dir
			rowClass := "dir-row"
			if current.Synced {
				rowClass += " synced"
			}

			rows = append(rows, app.Tr().Class(rowClass).Body(
				app.Td().Class("path-cell").Text(current.Path),
				app.Td().Body(
					app.Span().Class(syncStateClass(current.Synced)).Text(syncStateText(current.Synced)),
				),
				app.Td().Body(app.Div().Class("actions-cell").Body(renderDirActions(d, current))),
			))
		}
	}

	return app.Div().Class("table-wrap").Body(
		app.Table().Class("data-table").Body(
			app.THead().Body(
				app.Tr().Body(
					app.Th().Text("Path"),
					app.Th().Text("Synced"),
					app.Th().Text(""),
				),
			),
			app.TBody().Body(rows...),
		),
	)
}

func renderDirActions(d *dashboard, current dir) app.UI {
	if current.Synced {
		return app.Div().Class("inline-actions").Body(
			app.Button().
				Class("action-button secondary").
				Type("button").
				Text("Resync").
				OnClick(func(ctx app.Context, e app.Event) {
					d.handleSync(ctx, current.Path)
				}),
			app.Button().
				Class("action-button danger").
				Type("button").
				Text("Remove").
				OnClick(func(ctx app.Context, e app.Event) {
					d.handleRemove(ctx, current.Path)
				}),
		)
	}

	return app.Button().
		Class("action-button success").
		Type("button").
		Text("Sync").
		OnClick(func(ctx app.Context, e app.Event) {
			d.handleSync(ctx, current.Path)
		})
}

func (d *dashboard) handleSync(ctx app.Context, path string) {
	ctx.Async(func() {
		if err := postPath("/api/sync", path); err != nil {
			ctx.Dispatch(func(ctx app.Context) {
				d.pushError(err.Error())
			})
			return
		}

		ctx.Dispatch(func(ctx app.Context) {
			d.refreshAll(ctx)
		})
	})
}

func (d *dashboard) handleRemove(ctx app.Context, path string) {
	ctx.Async(func() {
		if err := postPath("/api/remove", path); err != nil {
			ctx.Dispatch(func(ctx app.Context) {
				d.pushError(err.Error())
			})
			return
		}

		ctx.Dispatch(func(ctx app.Context) {
			d.refreshAll(ctx)
		})
	})
}

func (d *dashboard) handleCancel(ctx app.Context, path string) {
	ctx.Async(func() {
		if err := postPath("/api/cancel", path); err != nil {
			ctx.Dispatch(func(ctx app.Context) {
				d.pushError(err.Error())
			})
			return
		}

		ctx.Dispatch(func(ctx app.Context) {
			d.refreshSyncs(ctx, false)
		})
	})
}

func (d *dashboard) refreshAll(ctx app.Context) {
	d.refreshDirs(ctx)
	d.refreshSyncs(ctx, false)
}

func (d *dashboard) refreshDirs(ctx app.Context) {
	ctx.Async(func() {
		result, err := fetchDirs()
		ctx.Dispatch(func(ctx app.Context) {
			if err != nil {
				d.pushError(err.Error())
				return
			}
			d.dirs = result
		})
	})
}

func (d *dashboard) refreshSyncs(ctx app.Context, refreshDirsOnCountChange bool) {
	ctx.Async(func() {
		result, err := fetchSyncs()
		ctx.Dispatch(func(ctx app.Context) {
			if err != nil {
				d.pushError(err.Error())
				return
			}

			countChanged := len(d.syncs) != len(result)
			d.syncs = result
			if refreshDirsOnCountChange && countChanged {
				d.refreshDirs(ctx)
			}
		})
	})
}

func (d *dashboard) schedulePoll(ctx app.Context) {
	if !d.active {
		return
	}

	ctx.After(2*time.Second, func(ctx app.Context) {
		if !d.active {
			return
		}

		d.refreshSyncs(ctx, true)
		d.schedulePoll(ctx)
	})
}

func (d *dashboard) pushError(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}

	if len(d.errors) > 0 && d.errors[len(d.errors)-1] == message {
		return
	}

	d.errors = append(d.errors, message)
	if len(d.errors) > 4 {
		d.errors = d.errors[len(d.errors)-4:]
	}
}

func (d *dashboard) dismissError(index int) {
	if index < 0 || index >= len(d.errors) {
		return
	}

	d.errors = append(d.errors[:index], d.errors[index+1:]...)
}

func fetchDirs() ([]dir, error) {
	var response dirsResponse
	if err := getJSON("/api/dirs", &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

func fetchSyncs() ([]syncItem, error) {
	var response syncsResponse
	if err := getJSON("/api/syncs", &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

func getJSON(url string, target any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeResponse(resp, target)
}

func postPath(url, path string) error {
	body, err := json.Marshal(pathRequest{Path: path})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeResponse(resp, nil)
}

func decodeResponse(resp *http.Response, target any) error {
	var payload baseResponse

	if target == nil {
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return err
		}
	} else {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return err
		}

		switch value := target.(type) {
		case *dirsResponse:
			payload = value.baseResponse
		case *syncsResponse:
			payload = value.baseResponse
		}
	}

	if resp.StatusCode != http.StatusOK {
		message := strings.TrimSpace(payload.Error)
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("%d: %s", resp.StatusCode, message)
	}

	if payload.Error != "" {
		return fmt.Errorf("%d: %s", resp.StatusCode, payload.Error)
	}

	return nil
}

func syncStateClass(synced bool) string {
	if synced {
		return "state-pill synced"
	}
	return "state-pill pending"
}

func syncStateText(synced bool) string {
	if synced {
		return "yes"
	}
	return "no"
}

func emptyDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func formatIEC(value uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	if value < 1024 {
		return fmt.Sprintf("%d%s", value, units[0])
	}

	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}

	if size >= 10 {
		return fmt.Sprintf("%.0f%s", size, units[unit])
	}
	return fmt.Sprintf("%.1f%s", size, units[unit])
}
