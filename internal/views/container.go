package views

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/derailed/k9s/internal/k8s"
	"github.com/derailed/k9s/internal/resource"
	"github.com/derailed/tview"
	"github.com/gdamore/tcell"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/tools/portforward"
)

const containerFmt = "[fg:bg:b]%s([hilite:bg:b]%s[fg:bg:-])"

type containerView struct {
	*logResourceView

	current igniter
	exitFn  func()
}

func newContainerView(app *appView, list resource.List, path string, exitFn func()) resourceViewer {
	title := skinTitle(fmt.Sprintf(containerFmt, "Containers", path), app.styles.Frame())

	v := containerView{
		logResourceView: newLogResourceView(title, app, list),
		exitFn:          exitFn,
	}
	v.path = &path
	v.containerFn = v.selectedContainer
	v.extraActionsFn = v.extraActions
	v.enterFn = v.viewLogs
	v.current = app.content.GetPrimitive("main").(igniter)

	return &v
}

func (v *containerView) init(ctx context.Context, ns string) {
	v.resourceView.init(ctx, ns)
	v.masterPage().colorerFn = containerColorer
}

func (v *containerView) extraActions(aa keyActions) {
	v.logResourceView.extraActions(aa)
	aa[KeyShiftF] = newKeyAction("PortForward", v.portFwdCmd, true)
	aa[KeyShiftL] = newKeyAction("Logs Previous", v.prevLogsCmd, true)
	aa[KeyS] = newKeyAction("Shell", v.shellCmd, true)
	aa[tcell.KeyEscape] = newKeyAction("Back", v.backCmd, false)
	aa[KeyP] = newKeyAction("Previous", v.backCmd, false)
	aa[KeyShiftC] = newKeyAction("Sort CPU", v.sortColCmd(6, false), true)
	aa[KeyShiftM] = newKeyAction("Sort MEM", v.sortColCmd(7, false), true)
	aa[KeyAltC] = newKeyAction("Sort CPU%", v.sortColCmd(8, false), true)
	aa[KeyAltM] = newKeyAction("Sort MEM%", v.sortColCmd(9, false), true)
}

func (v *containerView) selectedContainer() string {
	return v.selectedItem
}

func (v *containerView) viewLogs(app *appView, _, res, sel string) {
	status := trimCell(v.masterPage(), v.selectedRow, 3)
	if status == "Running" || status == "Completed" {
		v.showLogs(false)
		return
	}
	v.app.flash().err(errors.New("No logs available"))
}

// Handlers...

func (v *containerView) shellCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !v.rowSelected() {
		return evt
	}

	v.stopUpdates()
	shellIn(v.app, *v.path, v.selectedItem)
	v.restartUpdates()
	return nil
}

func (v *containerView) portFwdCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !v.rowSelected() {
		return evt
	}

	if _, ok := v.app.forwarders[fwFQN(*v.path, v.selectedItem)]; ok {
		v.app.flash().err(fmt.Errorf("A PortForward already exist on container %s", *v.path))
		return nil
	}

	state := trimCell(v.masterPage(), v.selectedRow, 3)
	if state != "Running" {
		v.app.flash().err(fmt.Errorf("Container %s is not running?", v.selectedItem))
		return nil
	}

	portC := trimCell(v.masterPage(), v.selectedRow, 10)
	ports := strings.Split(portC, ",")
	if len(ports) == 0 {
		v.app.flash().err(errors.New("Container exposes no ports"))
		return nil
	}

	var port string
	for _, p := range ports {
		log.Debug().Msgf("Checking port %q", p)
		if !isTCPPort(p) {
			continue
		}
		port = strings.TrimSpace(p)
		break
	}
	if port == "" {
		v.app.flash().warn("No valid TCP port found on this container. User will specify...")
		port = "MY_TCP_PORT!"
	}
	v.showPortFwdDialog(port, v.portForward)

	return nil
}

func (v *containerView) portForward(lport, cport string) {
	co := strings.TrimSpace(v.masterPage().GetCell(v.selectedRow, 0).Text)

	pf := k8s.NewPortForward(v.app.conn(), &log.Logger)
	ports := []string{lport + ":" + cport}
	fw, err := pf.Start(*v.path, co, ports)
	if err != nil {
		v.app.flash().err(err)
		return
	}

	log.Debug().Msgf(">>> Starting port forward %q %v", *v.path, ports)
	go v.runForward(pf, fw)
}

func (v *containerView) runForward(pf *k8s.PortForward, f *portforward.PortForwarder) {
	v.app.QueueUpdateDraw(func() {
		v.app.forwarders[pf.FQN()] = pf
		v.app.flash().infof("PortForward activated %s:%s", pf.Path(), pf.Ports()[0])
		v.dismissModal()
	})

	pf.SetActive(true)
	if err := f.ForwardPorts(); err != nil {
		v.app.flash().err(err)
		return
	}
	v.app.QueueUpdateDraw(func() {
		delete(v.app.forwarders, pf.FQN())
		pf.SetActive(false)
	})
}

func (v *containerView) dismissModal() {
	v.RemovePage("forward")
	v.switchPage("master")
}

func (v *containerView) backCmd(evt *tcell.EventKey) *tcell.EventKey {
	v.exitFn()
	return nil
}

func (v *containerView) showPortFwdDialog(port string, okFn func(lport, cport string)) {
	f := tview.NewForm()
	f.SetItemPadding(0)
	f.SetButtonsAlign(tview.AlignCenter).
		SetButtonBackgroundColor(tview.Styles.PrimitiveBackgroundColor).
		SetButtonTextColor(tview.Styles.PrimaryTextColor).
		SetLabelColor(tcell.ColorAqua).
		SetFieldTextColor(tcell.ColorOrange)

	p1, p2 := port, port
	f.AddInputField("Pod Port:", p1, 20, nil, func(port string) {
		p1 = port
	})
	f.AddInputField("Local Port:", p2, 20, nil, func(port string) {
		p2 = port
	})

	f.AddButton("OK", func() {
		okFn(stripPort(p2), stripPort(p1))
	})
	f.AddButton("Cancel", func() {
		v.app.flash().info("Canceled!!")
		v.dismissModal()
	})

	modal := tview.NewModalForm("<PortForward>", f)
	modal.SetDoneFunc(func(_ int, b string) {
		v.dismissModal()
	})
	v.AddPage("forward", modal, false, false)
	v.ShowPage("forward")
}
