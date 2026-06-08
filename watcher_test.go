package briefkasten_test

import (
	"testing"

	briefkasten "go.klarlabs.de/briefkasten"
)

func TestBuildWatcherSelectsBackend(t *testing.T) {
	dirCfg := &briefkasten.Config{Maildir: t.TempDir()}
	if w := dirCfg.BuildWatcher(); w == nil {
		t.Error("maildir backend should produce a watcher")
	}

	imapCfg := &briefkasten.Config{}
	imapCfg.IMAP.Addr = "imap.example:993"
	imapCfg.IMAP.Username = "u"
	imapCfg.IMAP.Password = "p"
	if w := imapCfg.BuildWatcher(); w == nil {
		t.Error("imap backend should produce a watcher")
	}
}
