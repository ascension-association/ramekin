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

	"github.com/ascension-association/ramekin/internal/agent"
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
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: ramekin device claim <name> [address]")
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
	if len(pos) == 2 {
		addr := pos[1]
		if !strings.Contains(addr, "://") {
			addr = "http://" + net.JoinHostPort(addr, "8095")
		}
		rec.URL = strings.TrimSuffix(addr, "/")
	} else if !existing {
		return fmt.Errorf("no device record for %s; give the device's address: ramekin device claim %s <ip-or-host>", name, name)
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
