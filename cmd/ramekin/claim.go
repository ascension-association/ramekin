package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
)

// deviceClaim gives a device its name and a signed identity. A device from
// a fleet image proves it is the board in front of you with the pairing code
// on its console; a device built for one board (`device init`) is claimed
// with its existing record's token instead, and keeps its token and gokrazy
// password.
func deviceClaim(ctx context.Context, args []string) error {
	fl := flags("device claim")
	code := fl.String("code", "", "pairing code shown on the device's console (prompted for if needed)")
	// Used for devices on official firmware, which get their settings here.
	channel := fl.String("channel", "", "release channel the device follows (default: first in ramekin.yaml)")
	healthHTTP := fl.String("health-http", "", "URL that must return 2xx for a new release to be accepted")
	healthTCP := fl.String("health-tcp", "", "host:port that must accept connections for a new release to be accepted")
	healthExec := fl.String("health-exec", "", "command run inside the container that must exit 0 for a new release to be accepted")
	statusURL := fl.String("status-url", "", "URL the device POSTs a signed JSON status report to after each update check")
	noFleetKey := fl.Bool("no-fleet-key", false, "don't give the device the project's fleet key: it can then only install releases published after its claim")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: ramekin device claim <name> [address]   (no address: find the unclaimed device on the local network)")
	}
	name := pos[0]
	p, err := loadProject()
	if err != nil {
		return err
	}
	signer, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}

	rec, recErr := resolveDevice(p, name)
	existing := recErr == nil
	switch {
	case len(pos) == 2:
		addr := pos[1]
		if !strings.Contains(addr, "://") {
			addr = "http://" + net.JoinHostPort(addr, "8095")
		}
		rec.URL = strings.TrimSuffix(addr, "/")
	case !existing:
		fmt.Println("looking for unclaimed devices on the local network...")
		found := findUnclaimed(ctx)
		switch len(found) {
		case 0:
			return fmt.Errorf("found no unclaimed device; give its address: ramekin device claim %s <ip-or-host>", name)
		case 1:
			rec.URL = found[0]
			fmt.Printf("found %s\n", strings.TrimPrefix(rec.URL, "http://"))
		default:
			return fmt.Errorf("found %d unclaimed devices (%s); say which: ramekin device claim %s <ip-or-host>", len(found), strings.Join(found, ", "), name)
		}
	}
	rec.name, rec.path = name, deviceRecordPath(p, name)

	info, err := claimInfo(ctx, rec.URL)
	if err != nil {
		return err
	}
	if info.Claimed {
		if info.Name == name && info.PublicKey == rec.PublicKey {
			fmt.Printf("%s is already claimed\n", name)
			return nil
		}
		return fmt.Errorf("that device is already claimed as %q", info.Name)
	}
	if info.Problem != "" {
		fmt.Printf("note: the device rejected its previous identity: %s\n", info.Problem)
	}
	if info.HardwareID == "" {
		fmt.Println("warning: this board exposes no hardware serial, so its identity can't be bound to it")
	}

	req := agent.ClaimRequest{Token: rec.Token}
	useToken := !info.NeedsCode && rec.Token != ""
	if !useToken {
		if *code == "" {
			fmt.Printf("Enter the pairing code shown on %s's console (or in its gokrazy log): ", rec.URL)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			*code = strings.TrimSpace(line)
		}
		req.Code = *code
		req.Token = randomHex(24)
		req.GokrazyPassword = randomHex(16)
	} else if pw := instancePassword(name); pw != "" {
		// Keep the gokrazy password, which moves from the image to /perm.
		req.GokrazyPassword = pw
	}
	if req.Token == "" {
		req.Token = randomHex(24)
	}
	if info.NeedsSettings {
		// Official firmware: the device takes its project from the claim.
		pub, err := os.ReadFile(p.Path(p.Signing.PubKey))
		if err != nil {
			return err
		}
		execArgs, err := splitArgs(*healthExec)
		if err != nil {
			return fmt.Errorf("--health-exec: %w", err)
		}
		update := config.Update{StatusURL: *statusURL}
		if p.Releases.GitHub != "" {
			update.Enabled, update.Channel, update.URL = true, firstNonEmpty(*channel, p.Channels[0]), p.ChannelURL()
		} else {
			fmt.Println("releases.github isn't set in ramekin.yaml, so the device takes apps you push (ramekin push), not published releases")
		}
		req.Settings = &config.Settings{
			SigningPubKey: string(pub),
			Update:        update,
			Container:     config.Container{Hostname: name},
			Health:        config.Health{HTTP: *healthHTTP, TCP: *healthTCP, Exec: execArgs},
		}
		req.GokrazyPassword = "" // official firmware runs gokrazy without one
	}
	if !*noFleetKey {
		// Encrypted to the device's own age key, so only it can read it.
		if k, err := keys.ReadAgeIdentity(p.Path("keys/fleet.agekey")); err == nil {
			rcpt, err := age.ParseX25519Recipient(info.AgeRecipient)
			if err != nil {
				return fmt.Errorf("device age key: %w", err)
			}
			var buf bytes.Buffer
			w, err := age.Encrypt(&buf, rcpt)
			if err != nil {
				return err
			}
			io.WriteString(w, k)
			if err := w.Close(); err != nil {
				return err
			}
			req.FleetKey = buf.Bytes()
		}
	}
	record := identity.Record{Name: name, PublicKey: info.PublicKey, AgeRecipient: info.AgeRecipient, HardwareID: info.HardwareID, ClaimedAt: time.Now().UTC()}
	if req.Record, err = manifest.Seal(record, signer); err != nil {
		return err
	}
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.URL+"/v1/claim", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if useToken {
		hreq.Header.Set("Authorization", "Bearer "+rec.Token)
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("device refused the claim: %s", strings.TrimSpace(string(answer)))
	}
	if err := identity.Verify(info.PublicKey, answer, resp.Header.Get(identity.SignatureHeader)); err != nil {
		return fmt.Errorf("the device's answer isn't signed with the key it presented: %w", err)
	}

	rec.Token, rec.PublicKey, rec.HardwareID, rec.AgeRecipient = req.Token, info.PublicKey, info.HardwareID, info.AgeRecipient
	if req.GokrazyPassword != "" {
		rec.GokrazyPassword = req.GokrazyPassword
	}
	if host, _, err := rec.hostPort(); err == nil {
		if mac := macFor(host); mac != "" {
			rec.MAC = mac
		}
	}
	if err := os.MkdirAll(filepath.Dir(rec.path), 0o700); err != nil {
		return err
	}
	if err := rec.save(); err != nil {
		return err
	}
	if err := keys.AppendRecipient(p.Path(p.Encrypt.RecipientsFile), info.AgeRecipient, "device "+name+" (claimed)"); err != nil {
		return err
	}
	fmt.Printf("claimed %s (hardware %s); it restarts to use its identity\n", name, info.HardwareID)
	fmt.Printf("its key was added to %s: releases published from now on are encrypted to it\n", p.Encrypt.RecipientsFile)
	return nil
}

func claimInfo(ctx context.Context, url string) (*agent.ClaimInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/claim", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s doesn't support claiming (%s); update its ramekind first", url, resp.Status)
	}
	var info agent.ClaimInfo
	return &info, json.NewDecoder(resp.Body).Decode(&info)
}

// instancePassword returns the gokrazy password of a device built for one
// board, from its gokrazy instance.
func instancePassword(name string) string {
	b, err := os.ReadFile(filepath.Join(gokrazyDir(""), name, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct{ Update struct{ HTTPPassword string } }
	json.Unmarshal(b, &cfg)
	return cfg.Update.HTTPPassword
}

// readVerified reads a device's answer. Once a device is claimed, every
// answer must carry its signature, made with the key recorded at claim time.
func (d *deviceRecord) readVerified(resp *http.Response) ([]byte, error) {
	b, err := io.ReadAll(resp.Body)
	if err != nil || d.PublicKey == "" {
		return b, err
	}
	if resp.StatusCode >= 400 && resp.Header.Get(identity.SignatureHeader) == "" {
		// Errors aren't signed: report them, but never as the device's answer.
		return nil, fmt.Errorf("%s: %s (unsigned)", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := identity.Verify(d.PublicKey, b, resp.Header.Get(identity.SignatureHeader)); err != nil {
		return nil, fmt.Errorf("identity check failed for %s: %w", firstNonEmpty(d.name, d.URL), err)
	}
	return b, nil
}

// status fetches and checks a device's status: signed (once claimed), for
// this device, and answering this request.
func (d *deviceRecord) status(ctx context.Context, timeout time.Duration) (*agent.DeviceStatus, []byte, error) {
	nonce := randomHex(12)
	resp, err := d.do(ctx, http.MethodGet, "/v1/status?nonce="+nonce, nil, timeout)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	b, err := d.readVerified(resp)
	if err != nil {
		return nil, nil, err
	}
	var st agent.DeviceStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, nil, err
	}
	if d.PublicKey != "" && (st.Nonce != nonce || (d.name != "" && st.Device != d.name)) {
		return nil, nil, fmt.Errorf("identity check failed: the answer is for %q, not this request to %s", st.Device, d.name)
	}
	return &st, b, nil
}

// deviceRelease factory-resets a claimed device: it forgets its identity,
// project settings, apps and app data, keeps its firmware, and restarts
// unclaimed, ready to be claimed again (by this project or another).
func deviceRelease(ctx context.Context, args []string) error {
	fl := flags("device release")
	yes := fl.Bool("yes", false, "don't ask for confirmation")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device release <name>")
	}
	name := pos[0]
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, name)
	if err != nil {
		return err
	}
	if d.PublicKey == "" {
		return fmt.Errorf("%s isn't claimed", name)
	}
	if !*yes {
		fmt.Printf("This erases %s's identity, settings, apps and app data; it keeps its firmware and restarts unclaimed.\nType the device's name to confirm: ", name)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != name {
			return errors.New("not confirmed")
		}
	}
	signer, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	body, err := manifest.Seal(agent.ReleaseRequest{Release: name, At: time.Now().UTC()}, signer)
	if err != nil {
		return err
	}
	resp, err := d.do(ctx, http.MethodDelete, "/v1/claim", bytes.NewReader(body), time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	os.Remove(d.path)
	if err := removeRecipient(p.Path(p.Encrypt.RecipientsFile), d.AgeRecipient); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	fmt.Printf("released %s: it restarts unclaimed and shows a new pairing code. Its record and key were removed from this project.\n", name)
	return nil
}

// removeRecipient drops an age recipient (and the comment line before it)
// from a recipients file.
func removeRecipient(file, recipient string) error {
	if recipient == "" {
		return nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	for i, l := range lines {
		if strings.TrimSpace(l) == recipient {
			if len(out) > 0 && strings.HasPrefix(out[len(out)-1], "#") && i > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, l)
	}
	return os.WriteFile(file, []byte(strings.Join(out, "\n")), 0o644)
}

// deviceInstall installs a device that runs from an SD card onto its
// internal storage (eMMC or NVMe), with its identity, settings, apps and
// data, so the card isn't needed afterwards.
func deviceInstall(ctx context.Context, args []string) error {
	fl := flags("device install")
	to := fl.String("to", "", "emmc, nvme, or a disk name from `ramekin status` (e.g. mmcblk0)")
	overwrite := fl.Bool("overwrite", false, "erase the disk's existing partitions (e.g. a preinstalled OS)")
	yes := fl.Bool("yes", false, "don't ask for confirmation")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" {
		return errors.New("usage: ramekin device install <name> --to emmc|nvme [--overwrite]")
	}
	name := pos[0]
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, name)
	if err != nil {
		return err
	}
	if d.PublicKey == "" {
		return fmt.Errorf("%s isn't claimed", name)
	}
	st, _, err := d.status(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	var target *agent.Disk
	var boot string
	for i, disk := range st.Disks {
		if disk.Boot {
			boot = disk.Name + " (" + disk.Kind + ")"
		}
		if !disk.Boot && (disk.Name == *to || disk.Kind == *to) {
			if target != nil && disk.Name != *to {
				return fmt.Errorf("%s has more than one %s disk; name one (e.g. --to %s)", name, *to, disk.Name)
			}
			target = &st.Disks[i]
		}
	}
	if target == nil {
		var have []string
		for _, disk := range st.Disks {
			have = append(have, fmt.Sprintf("%s (%s, %.1f GB)", disk.Name, disk.Kind, float64(disk.SizeBytes)/1e9))
		}
		return fmt.Errorf("%s has no %s disk to install to; it has %s", name, *to, strings.Join(have, ", "))
	}
	if !*yes {
		fmt.Printf("This installs %s's firmware onto %s (%s, %.1f GB, %s) and copies its identity, settings, apps and data there.\n",
			name, target.Name, target.Kind, float64(target.SizeBytes)/1e9, firstNonEmpty(target.Model, "no model name"))
		if *overwrite {
			fmt.Printf("Everything on %s is erased. ", target.Name)
		}
		fmt.Printf("It now runs from %s, which is left as it is.\nType the device's name to confirm: ", boot)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != name {
			return errors.New("not confirmed")
		}
	}
	signer, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	body, err := manifest.Seal(agent.InstallRequest{Device: name, Target: target.Name, Overwrite: *overwrite, At: time.Now().UTC()}, signer)
	if err != nil {
		return err
	}
	fmt.Printf("installing onto %s: downloading and writing the firmware's disk image, then copying data (a few minutes)...\n", target.Name)
	resp, err := d.do(ctx, http.MethodPost, "/v1/install", bytes.NewReader(body), 30*time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var res agent.InstallResult
	json.Unmarshal(b, &res)
	if len(res.Erased) > 0 {
		fmt.Printf("erased %s's previous partitions: %s\n", res.Target, strings.Join(res.Erased, ", "))
	}
	fmt.Printf(`installed firmware %s onto %s. The device is powering off.
Remove the SD card, then power it on: it boots from %s, keeps its identity
and apps, and grows its storage to fill the disk. Keep the card as a fallback.
`, res.Firmware, res.Target, res.Target)
	return nil
}
