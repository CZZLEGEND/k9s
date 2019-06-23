package views

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/resource"
	"github.com/derailed/tview"
	"github.com/fsnotify/fsnotify"
	"github.com/gdamore/tcell"
	"github.com/rs/zerolog/log"
)

const (
	forwardTitle    = "Port Forwards"
	forwardTitleFmt = " [aqua::b]%s([fuchsia::b]%d[fuchsia::-])[aqua::-] "
	promptPage      = "prompt"
)

type forwardView struct {
	*tview.Pages

	app    *appView
	cancel context.CancelFunc
	bench  *benchmark
}

var _ resourceViewer = &forwardView{}

func newForwardView(ns string, app *appView, list resource.List) resourceViewer {
	v := forwardView{
		Pages: tview.NewPages(),
		app:   app,
	}

	tv := newTableView(app, forwardTitle)
	tv.SetBorderFocusColor(tcell.ColorDodgerBlue)
	tv.SetSelectedStyle(tcell.ColorWhite, tcell.ColorDodgerBlue, tcell.AttrNone)
	tv.colorerFn = forwardColorer
	tv.currentNS = ""
	v.AddPage("table", tv, true, true)
	v.registerActions()

	return &v
}

func (v *forwardView) setEnterFn(enterFn)          {}
func (v *forwardView) setColorerFn(colorerFn)      {}
func (v *forwardView) setDecorateFn(decorateFn)    {}
func (v *forwardView) setExtraActionsFn(actionsFn) {}

// Init the view.
func (v *forwardView) init(ctx context.Context, _ string) {
	if err := watchK9sDir(ctx, v.app, v.reload); err != nil {
		v.app.flash().errf("RuRoh! Unable to watch benchmarks directory %s : %s", config.K9sHome, err)
	}

	tv := v.getTV()
	v.refresh()
	tv.sortCol.index, tv.sortCol.asc = tv.nameColIndex()+6, true
	tv.refresh()
	tv.Select(1, 0)
	v.app.SetFocus(tv)
	v.app.setHints(v.hints())
}

func (v *forwardView) getTV() *tableView {
	if vu, ok := v.GetPrimitive("table").(*tableView); ok {
		return vu
	}
	return nil
}

func (v *forwardView) reload() {
	path := benchConfig(v.app.config.K9s.CurrentCluster)
	log.Debug().Msgf("Reloading config %s", path)
	if err := v.app.bench.Reload(path); err != nil {
		v.app.flash().err(err)
	}
	v.refresh()
}

func (v *forwardView) refresh() {
	tv := v.getTV()
	tv.update(v.hydrate())
	v.app.SetFocus(tv)
	tv.resetTitle()
}

func (v *forwardView) registerActions() {
	tv := v.getTV()
	tv.actions[tcell.KeyEnter] = newKeyAction("Goto", v.gotoBenchCmd, true)
	tv.actions[tcell.KeyCtrlB] = newKeyAction("Bench", v.benchCmd, true)
	tv.actions[KeyAltB] = newKeyAction("Bench Stop", v.benchStopCmd, true)
	tv.actions[tcell.KeyCtrlD] = newKeyAction("Delete", v.deleteCmd, true)
	tv.actions[KeySlash] = newKeyAction("Filter", tv.activateCmd, false)
	tv.actions[KeyP] = newKeyAction("Previous", v.app.prevCmd, false)
	tv.actions[KeyShiftP] = newKeyAction("Sort Ports", v.sortColCmd(2, true), true)
	tv.actions[KeyShiftU] = newKeyAction("Sort URL", v.sortColCmd(4, true), true)
}

func (v *forwardView) getTitle() string {
	return forwardTitle
}

func (v *forwardView) sortColCmd(col int, asc bool) func(evt *tcell.EventKey) *tcell.EventKey {
	return func(evt *tcell.EventKey) *tcell.EventKey {
		tv := v.getTV()
		tv.sortCol.index, tv.sortCol.asc = tv.nameColIndex()+col, asc
		v.refresh()

		return nil
	}
}

func (v *forwardView) gotoBenchCmd(evt *tcell.EventKey) *tcell.EventKey {
	v.app.gotoResource("be", true)

	return nil
}

func (v *forwardView) benchStopCmd(evt *tcell.EventKey) *tcell.EventKey {
	if v.bench != nil {
		log.Debug().Msg(">>> Benchmark canceled!!")
		v.app.status(flashErr, "Benchmark Camceled!")
		v.bench.cancel()
	}
	v.app.statusReset()

	return nil
}

func (v *forwardView) benchCmd(evt *tcell.EventKey) *tcell.EventKey {
	sel := v.getSelectedItem()
	if sel == "" {
		return nil
	}

	if v.bench != nil {
		v.app.flash().err(errors.New("Only one benchmark allowed at a time"))
		return nil
	}

	tv := v.getTV()
	r, _ := tv.GetSelection()
	cfg, co := defaultConfig(), trimCell(tv, r, 2)
	if b, ok := v.app.bench.Benchmarks.Containers[containerID(sel, co)]; ok {
		cfg = b
	}
	cfg.Name = sel

	base := trimCell(tv, r, 4)
	var err error
	if v.bench, err = newBenchmark(base, cfg); err != nil {
		v.app.flash().errf("Bench failed %v", err)
		v.app.statusReset()
		return nil
	}

	v.app.status(flashWarn, "Benchmark in progress...")
	log.Debug().Msg("Bench starting...")
	go v.runBenchmark()

	return nil
}

func (v *forwardView) runBenchmark() {
	v.bench.run(v.app.config.K9s.CurrentCluster, func() {
		log.Debug().Msg("Bench Completed!")
		v.app.QueueUpdate(func() {
			if v.bench.canceled {
				v.app.status(flashInfo, "Benchmark canceled")
			} else {
				v.app.status(flashInfo, "Benchmark Completed!")
				v.bench.cancel()
			}
			v.bench = nil
			go func() {
				<-time.After(2 * time.Second)
				v.app.QueueUpdate(func() { v.app.statusReset() })
			}()
		})
	})
}

func (v *forwardView) getSelectedItem() string {
	tv := v.getTV()
	r, _ := tv.GetSelection()
	if r == 0 {
		return ""
	}
	return fwFQN(fqn(trimCell(tv, r, 0), trimCell(tv, r, 1)), trimCell(tv, r, 2))
}

func (v *forwardView) deleteCmd(evt *tcell.EventKey) *tcell.EventKey {
	tv := v.getTV()
	if !tv.cmdBuff.empty() {
		tv.cmdBuff.reset()
		return nil
	}

	sel := v.getSelectedItem()
	if sel == "" {
		return nil
	}

	showModal(v.Pages, fmt.Sprintf("Delete PortForward `%s?", sel), "table", func() {
		fw, ok := v.app.forwarders[sel]
		if !ok {
			log.Debug().Msgf("Unable to find forwarder %s", sel)
			return
		}
		fw.Stop()
		delete(v.app.forwarders, sel)

		log.Debug().Msgf("PortForwards after delete: %#v", v.app.forwarders)
		v.getTV().update(v.hydrate())
		v.app.flash().infof("PortForward %s deleted!", sel)
	})

	return nil
}

func (v *forwardView) backCmd(evt *tcell.EventKey) *tcell.EventKey {
	if v.cancel != nil {
		v.cancel()
	}

	tv := v.getTV()
	if tv.cmdBuff.isActive() {
		tv.cmdBuff.reset()
	} else {
		v.app.inject(v.app.content.GetPrimitive("main").(igniter))
	}

	return nil
}

func (v *forwardView) hints() hints {
	return v.getTV().actions.toHints()
}

func (v *forwardView) hydrate() resource.TableData {
	data := initHeader(len(v.app.forwarders))
	dc, dn := v.app.bench.Benchmarks.Defaults.C, v.app.bench.Benchmarks.Defaults.N
	for _, f := range v.app.forwarders {
		c, n, cfg := loadConfig(dc, dn, containerID(f.Path(), f.Container()), v.app.bench.Benchmarks.Containers)

		ports := strings.Split(f.Ports()[0], ":")
		ns, na := namespaced(f.Path())
		fields := resource.Row{
			ns,
			na,
			f.Container(),
			strings.Join(f.Ports(), ","),
			urlFor(cfg, f.Container(), ports[0]),
			asNum(c),
			asNum(n),
			f.Age(),
		}
		data.Rows[f.Path()] = &resource.RowEvent{
			Action: resource.New,
			Fields: fields,
			Deltas: fields,
		}
	}

	return data
}

func (v *forwardView) resetTitle() {
	v.SetTitle(fmt.Sprintf(forwardTitleFmt, forwardTitle, v.getTV().GetRowCount()-1))
}

// ----------------------------------------------------------------------------
// Helpers...

func defaultConfig() config.BenchConfig {
	return config.BenchConfig{
		C: config.DefaultC,
		N: config.DefaultN,
		HTTP: config.HTTP{
			Method: config.DefaultMethod,
			Path:   "/",
		},
	}
}

func initHeader(rows int) resource.TableData {
	return resource.TableData{
		Header:    resource.Row{"NAMESPACE", "NAME", "CONTAINER", "PORTS", "URL", "C", "N", "AGE"},
		NumCols:   map[string]bool{"C": true, "N": true},
		Rows:      make(resource.RowEvents, rows),
		Namespace: resource.AllNamespaces,
	}
}

func loadConfig(dc, dn int, id string, cc map[string]config.BenchConfig) (int, int, config.BenchConfig) {
	c, n := dc, dn
	cfg, ok := cc[id]
	if !ok {
		return c, n, cfg
	}

	if cfg.C != 0 {
		c = cfg.C
	}
	if cfg.N != 0 {
		n = cfg.N
	}

	return c, n, cfg
}

func showModal(pv *tview.Pages, msg, back string, ok func()) {
	m := tview.NewModal().
		AddButtons([]string{"Cancel", "OK"}).
		SetTextColor(tcell.ColorFuchsia).
		SetText(msg).
		SetDoneFunc(func(_ int, b string) {
			if b == "OK" {
				ok()
			}
			dismissModal(pv, back)
		})
	m.SetTitle("<Confirm>")
	pv.AddPage(promptPage, m, false, false)
	pv.ShowPage(promptPage)
}

func dismissModal(pv *tview.Pages, page string) {
	pv.RemovePage(promptPage)
	pv.SwitchToPage(page)
}

func watchK9sDir(ctx context.Context, app *appView, cb func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	go updateBenchConfig(ctx, w, app, cb)

	return w.Add(config.K9sHome)
}

func updateBenchConfig(ctx context.Context, w *fsnotify.Watcher, app *appView, cb func()) {
	file := benchConfig(app.config.K9s.CurrentCluster)
	for {
		select {
		case evt := <-w.Events:
			if file == "" || evt.Name == file {
				log.Debug().Msgf("Capuring Event %#v", evt)
				app.QueueUpdateDraw(func() {
					cb()
				})
			}
		case err := <-w.Errors:
			log.Info().Err(err).Msgf("FS %s watcher failed", file)
			return
		case <-ctx.Done():
			log.Debug().Msgf("<<FS %s WATCHER DONE>>", file)
			w.Close()
			return
		}
	}
}

func benchConfig(cluster string) string {
	return filepath.Join(config.K9sHome, config.K9sBench+"-"+cluster+".yml")
}
