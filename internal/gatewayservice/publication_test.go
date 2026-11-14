package gatewayservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestServiceInstalledPathRemainsAuthoritativeAcrossCallerEnvironmentChanges(t *testing.T) {
	for _, platform := range []Platform{Linux, MacOS} {
		t.Run(string(platform), func(t *testing.T) { testInstalledServicePath(t, platform) })
	}
}

func testInstalledServicePath(t *testing.T, platform Platform) {
	t.Helper()
	f := newServiceFixture(t, platform)
	c := f.manager.config
	c.Path = `/old/provider %/bin:/old/"quoted"/bin:/old/back\slash:/usr/bin`
	manager, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(manager.unitPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []string{"/new/bin", "invalid-relative-path"} {
		c.Path = current
		manager, err = New(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []func(context.Context) (Status, error){manager.Status, manager.Install, manager.Start, manager.Stop} {
			if _, err = action(f.ctx); err != nil {
				t.Fatalf("caller PATH changed saved installation: %v", err)
			}
		}
		data, readErr := os.ReadFile(manager.unitPath())
		if readErr != nil || string(data) != string(original) {
			t.Fatal("retry rewrote authoritative installed PATH")
		}
	}
	if _, err = manager.Uninstall(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServiceAtomicPublicationReconcilesOnlyPrivateUnpublishedStage(t *testing.T) {
	for _, content := range []string{"partial", "complete"} {
		t.Run(content, func(t *testing.T) {
			f := newServiceFixture(t, Linux)
			if err := os.Mkdir(f.manager.config.Directory, 0o700); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(f.manager.config.Directory, "."+f.manager.filename+".installing")
			data := []byte("unfinished write")
			if content == "complete" {
				data = f.manager.rendered
			}
			if err := os.WriteFile(stage, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.Install(f.ctx); err != nil {
				t.Fatal(err)
			}
			assertPublishedSingleLink(t, f.manager)
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stage remains after commit: %v", err)
			}
		})
	}
}

func assertPublishedSingleLink(t *testing.T, manager *Manager) {
	t.Helper()
	info, err := os.Lstat(manager.unitPath())
	if err != nil {
		t.Fatal(err)
	}
	stat, valid := info.Sys().(*syscall.Stat_t)
	if !valid || stat.Nlink != 1 {
		t.Fatal("publication left an additional hard link")
	}
	if present, err := manager.readUnit(); err != nil || !present {
		t.Fatalf("committed unit unreadable: %v", err)
	}
}

func TestServiceAtomicPublicationNeverReplacesForeignWinner(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if err := os.Mkdir(f.manager.config.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign concurrent winner\n")
	if err := os.WriteFile(f.manager.unitPath(), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.installUnit(); !errors.Is(err, os.ErrExist) {
		t.Fatalf("no-replace publication accepted winner: %v", err)
	}
	data, err := os.ReadFile(f.manager.unitPath())
	if err != nil || string(data) != string(foreign) {
		t.Fatal("foreign winner was replaced")
	}
	stage := filepath.Join(f.manager.config.Directory, "."+f.manager.filename+".installing")
	if _, err = os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication left owned stage: %v", err)
	}
}

func TestServiceUnpublishedStageRefusesSymlinksAndExtraLinks(t *testing.T) {
	for _, link := range []string{"symlink", "hardlink"} {
		t.Run(link, func(t *testing.T) {
			f := newServiceFixture(t, Linux)
			if err := os.Mkdir(f.manager.config.Directory, 0o700); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(f.manager.config.Directory, "foreign")
			if err := os.WriteFile(foreign, []byte("preserved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(f.manager.config.Directory, "."+f.manager.filename+".installing")
			var err error
			if link == "symlink" {
				err = os.Symlink(foreign, stage)
			} else {
				err = os.Link(foreign, stage)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.manager.Install(f.ctx); !errors.Is(err, ErrForeignService) {
				t.Fatalf("ambiguous staging accepted: %v", err)
			}
			if _, err = os.Lstat(stage); err != nil {
				t.Fatal("foreign stage was removed")
			}
		})
	}
}

func TestServiceStillRefusesPreexistingMultiLinkedUnit(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.manager.config.Directory, "unowned-alias")
	if err := os.Link(f.manager.unitPath(), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, ErrForeignService) {
		t.Fatalf("multi-linked installed unit was adopted: %v", err)
	}
	if _, err := os.Stat(alias); err != nil {
		t.Fatal("refusal removed foreign alias")
	}
}
