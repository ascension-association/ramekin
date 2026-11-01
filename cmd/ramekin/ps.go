package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
)

// cmdPs lists the processes running on a device.
func cmdPs(ctx context.Context, args []string) error {
	fl := flags("ps")
	kernel := fl.Bool("kernel", false, "include kernel threads")
	asJSON := fl.Bool("json", false, "print the device's (signed) JSON answer")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin ps <device> [--kernel] [--json]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, pos[0])
	if err != nil {
		return err
	}
	nonce := randomHex(12)
	path := "/v1/processes?nonce=" + nonce
	if *kernel {
		path += "&kernel=1"
	}
	resp, err := d.do(ctx, http.MethodGet, path, nil, time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("this device's ramekind can't list processes yet: update its firmware")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	b, err := d.readVerified(resp)
	if err != nil {
		return err
	}
	var l agent.ProcessList
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	if d.PublicKey != "" && (l.Nonce != nonce || (d.name != "" && l.Device != d.name)) {
		return fmt.Errorf("identity check failed: the answer is for %q, not this request to %s", l.Device, d.name)
	}
	if *asJSON {
		_, err = os.Stdout.Write(b)
		return err
	}
	printProcesses(os.Stdout, l)
	return nil
}

// printProcesses lists the device's own processes (gokrazy and Ramekin),
// then the app's, which run from its SIF in their own container, then any
// kernel threads.
func printProcesses(w io.Writer, l agent.ProcessList) {
	var host, app, kernel []agent.ProcInfo
	for _, p := range l.Processes {
		switch {
		case p.Kernel:
			kernel = append(kernel, p)
		case p.Container:
			app = append(app, p)
		default:
			host = append(host, p)
		}
	}
	section := func(title string, ps []agent.ProcInfo) {
		fmt.Fprintf(w, "%s\n", title)
		if len(ps) == 0 {
			fmt.Fprintln(w, "  (none)")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PID\tPPID\tUID\tSTATE\tRSS\tCPU\tTHR\tCOMMAND")
		for _, p := range ps {
			cmd := strings.Join(p.Args, " ")
			if cmd == "" {
				cmd = "[" + p.Name + "]"
			}
			if p.Exe != "" && (len(p.Args) == 0 || p.Args[0] != p.Exe) {
				cmd += "  (" + p.Exe + ")"
			}
			fmt.Fprintf(tw, "  %d\t%d\t%d\t%s\t%s\t%.1fs\t%d\t%s\n",
				p.PID, p.PPID, p.UID, p.State, mib(p.RSSBytes), p.CPUSeconds, p.Threads, cmd)
		}
		tw.Flush()
	}
	section("Device (gokrazy and Ramekin):", host)
	fmt.Fprintln(w)
	title := "App (in its container, from its SIF):"
	if l.App != "" {
		title = fmt.Sprintf("App %s (in its container, from its SIF):", l.App)
	}
	section(title, app)
	if len(kernel) > 0 {
		fmt.Fprintln(w)
		section(fmt.Sprintf("Kernel threads (%d):", len(kernel)), kernel)
	}
}

func mib(n uint64) string {
	return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
}
