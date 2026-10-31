package agent

import (
	"context"
	"log"
	"os"
	"time"
)

// KeepWatchdog pings the hardware watchdog if nothing else holds it.
//
// gokrazy normally holds /dev/watchdog. But on a test boot its init opens
// the watchdog before it has mounted /dev on the new root, fails, and leaves
// the watchdog the previous init armed unattended, so the board resets
// within its timeout, before the new image can be confirmed. Opening it here
// succeeds only in that case (otherwise the device is busy). A new image
// that hangs before ramekind starts is still reset by the watchdog, which
// returns the device to its previous image.
func KeepWatchdog(ctx context.Context, logger *log.Logger) {
	f, err := os.OpenFile("/dev/watchdog", os.O_WRONLY, 0)
	if err != nil {
		return // held by gokrazy, or there is none
	}
	logger.Print("the hardware watchdog was unattended; ramekind is keeping it")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if _, err := f.Write([]byte{0}); err != nil {
			logger.Printf("watchdog: %v", err)
		}
		select {
		case <-ctx.Done():
			// Leave it armed (no magic close): gokrazy is shutting down too.
			f.Close()
			return
		case <-t.C:
		}
	}
}
