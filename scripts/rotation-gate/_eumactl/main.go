// Command eumactl performs the fleet-administration acts a rotation needs,
// against a real Eumaeus store, without a provider key.
//
// It is compiled and run INSIDE an Eumaeus checkout:
//
//	cd $EUMAEUS_DIR && go run /path/to/eumactl/main.go adopt …
//
// which is why it lives here as a single file with no go.mod of its own. `go
// run` on an absolute path resolves imports against the module of the working
// directory, so this file links against whatever Eumaeus is checked out — the
// commit under test, not a version pinned here. That is the whole point: the
// gate is worth running because the server half is real.
//
// The directory begins with an underscore because that is how the Go tool is
// told to ignore it. Without that, `go build ./...` in this repository tries
// to resolve these imports against this module, does not find them, and every
// build on every machine without an Eumaeus checkout fails — which is the
// opposite of a gate that skips cleanly. An explicit file path is exempt from
// the rule, which is why `go run` on it still works.
//
// # Why this exists at all
//
// Every administrative act in a rotation is reachable from Eumaeus's own
// command line, and each one of them calls the provisioner:
//
//	plan, _ := sess.Backup.PlanAdoption(…)   // unit 1 — pure domain
//	out, _  := prov.Provision(ctx, plan.Request())
//	repo, _ := sess.Backup.SettleProvision(ctx, plan, out)   // unit 2 — pure domain
//
// The middle line creates a bucket and mints two IAM identities, fresh, every
// time. For a gate that runs on a schedule that is real money and a real leak:
// a key per run, in a log, for a bucket nothing will ever delete — nothing in
// Eumaeus can delete a bucket, by policy, so every run would leave one behind
// for a person to clean up by hand.
//
// So this program supplies `out` itself, from storage the gate already made
// and will reuse. Units one and two are Eumaeus's own code, unmodified, and
// what is skipped is the one call whose behaviour is Eumaeus's business with
// Wasabi rather than anything sion-backup can observe. Everything the client
// actually reads — the state a second repository is created in, what
// `expect_empty` answers, what `created_at` means, how the card resolves — is
// decided in unit two and afterwards, and runs here for real.
//
// # What it may not become
//
// A second way to administer a fleet. It writes through [backupbus.Business]
// and never through SQL, so a rule Eumaeus enforces is a rule this obeys, and
// a gate that passes because the helper took a shortcut is a gate that proves
// nothing. The one read that goes to the store directly is `show`, which
// exists to let the gate assert on rows the API deliberately does not serve.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jroedel/eumaeus/business/domain/backup/backupbus"
	"github.com/jroedel/eumaeus/business/domain/backup/stores/backupdb"
	"github.com/jroedel/eumaeus/foundation/paths"
	"github.com/jroedel/eumaeus/foundation/sqldb"
	"github.com/jroedel/eumaeus/foundation/vault"
)

// backupVault is the fleet store's name, and it is duplicated from
// cmd/eumaeus/fleet.go because that is package main and cannot be imported.
//
// A constant copied across a repository boundary is a thing that rots
// silently, so the gate does not rely on this being right: it runs `eumaeus
// backup init` first, and a vault under any other name would leave this one
// absent and fail here rather than quietly building a second store.
const backupVault = "backup"

// keepGenerations matches cmd/eumaeus. Same argument as above.
const keepGenerations = 3

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "eumactl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: eumactl <person|adopt|provision|code|retire|show> [flags]")
	}

	ctx := context.Background()

	switch args[0] {
	case "person":
		return person(ctx, args[1:])
	case "adopt":
		return settle(ctx, args[1:], true)
	case "provision":
		return settle(ctx, args[1:], false)
	case "code":
		return code(ctx, args[1:])
	case "retire":
		return retire(ctx, args[1:])
	case "show":
		return show(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// unit is backupUnitOver from cmd/eumaeus/fleet.go, reproduced.
//
// Twenty lines rather than an import because the original is in package main.
// It opens the same vault by the same name with the same key file, so the
// server started beside this sees every row this writes.
func unit(ctx context.Context, fn func(*backupbus.Business, backupbus.Storer) error) error {
	p, err := paths.Resolve()
	if err != nil {
		return err
	}

	if !vault.HasServerKey(p.BackupKey) {
		return fmt.Errorf("no fleet key at %s: run \"eumaeus backup init\" first", p.BackupKey)
	}

	v, err := vault.New(vault.Config{
		Name: backupVault,
		Dir:  p.Vaults,
		Keep: keepGenerations,
		Keys: vault.FileKey(p.BackupKey),
		Log:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return err
	}

	return v.Do(ctx, func(db *sqldb.DB) error {
		// No Migrate here. "eumaeus backup init" made the store and the server
		// migrates it; a third migrator would be a third opinion about the
		// schema, and this program is a guest.
		store := backupdb.NewStore(db)

		return fn(backupbus.NewBusiness(store, time.Now), store)
	})
}

func person(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("person", flag.ExitOnError)
	name := fs.String("name", "", "the person's name")
	email := fs.String("email", "", "where their warnings go")
	admin := fs.Bool("admin", false, "an admin rather than an owner")

	if err := fs.Parse(args); err != nil {
		return err
	}

	role := backupbus.RoleOwner
	if *admin {
		role = backupbus.RoleAdmin
	}

	return unit(ctx, func(b *backupbus.Business, _ backupbus.Storer) error {
		p, err := b.AddPerson(ctx, *name, *email, role)
		if err != nil {
			return err
		}

		fmt.Println(p.ID)

		return nil
	})
}

// settle runs both units with a Provisioned assembled from flags.
//
// The two commands differ only in which planner runs, and they are one
// function because the interesting half — what Eumaeus decides to do with the
// result — is identical, and the gate's whole subject is that decision. A
// second repository for a machine that has one becomes `offered` rather than
// `active`, and nothing here asks for that or knows how it happens.
func settle(ctx context.Context, args []string, adopting bool) error {
	name := "provision"
	if adopting {
		name = "adopt"
	}

	fs := flag.NewFlagSet(name, flag.ExitOnError)

	owner := fs.Int64("owner", 0, "the person's id, from \"eumactl person\"")
	node := fs.String("node", "", "the node id")
	bucket := fs.String("bucket", "", "the bucket name")
	url := fs.String("url", "", "the repository URL, stored verbatim")
	provider := fs.String("provider", "wasabi", "the provider's name")
	region := fs.String("region", "", "the provider's region")
	by := fs.String("by", "the rotation gate", "who is doing this, for the disclosure log")

	keyID := fs.String("key-id", "", "the machine identity's access key")
	secret := fs.String("secret", "", "the machine identity's secret")
	restoreID := fs.String("restore-key-id", "", "the restore identity's access key")
	restoreSecret := fs.String("restore-secret", "", "the restore identity's secret")

	// Adoption only.
	password := fs.String("password", "", "the password already opening that repository (adopt only)")
	since := fs.String("history-since", "", "RFC3339 date where the snapshots start (adopt only)")
	snapshots := fs.Int("snapshots", 0, "how many snapshots are already there (adopt only)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	var horizon time.Time

	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return fmt.Errorf("-history-since: %w", err)
		}

		horizon = t
	}

	out := backupbus.Provisioned{
		Provider: *provider,
		Region:   *region,
		Bucket:   *bucket,
		URL:      *url,
		Machine:  backupbus.AccessKey{AccessKeyID: *keyID, SecretAccessKey: *secret},
		Restore:  backupbus.AccessKey{AccessKeyID: *restoreID, SecretAccessKey: *restoreSecret},
	}

	// Refused here rather than left to fail later, because a half-filled key
	// reaches restic as an authentication error at 1am and reads like a
	// rotation bug. Eumaeus has its own check; this one names the flag.
	if out.Machine.Empty() || out.Restore.Empty() {
		return errors.New("both identities need a key id and a secret: see -key-id and -restore-key-id")
	}

	// Two units, exactly as provisioning requires — but with no network call
	// between them, so they can share one here. They are still two calls in
	// the order Eumaeus documents, which is what the gate is checking.
	return unit(ctx, func(b *backupbus.Business, _ backupbus.Storer) error {
		var (
			plan backupbus.ProvisionPlan
			err  error
		)

		if adopting {
			plan, err = b.PlanAdoption(ctx, *owner, *node, *bucket, *password, *by, horizon, *snapshots)
		} else {
			plan, err = b.PlanProvision(ctx, *owner, *node, *bucket, *by)
		}

		if err != nil {
			return err
		}

		repo, err := b.SettleProvision(ctx, plan, out)
		if err != nil {
			return err
		}

		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"id":            repo.ID,
			"url":           repo.URL,
			"state":         string(repo.State),
			"adopted":       repo.Adopted,
			"created_at":    repo.CreatedAt,
			"history_since": repo.HistorySince,

			// The password this machine's repository is opened with. Printed
			// because an adoption is given one and a provision generates one,
			// and the gate needs it to run restic against the bucket itself
			// — which is the only way to check that what Eumaeus serves is
			// what actually opens the repository.
			"restic_password": plan.ResticPassword,
		})
	})
}

func code(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("code", flag.ExitOnError)
	node := fs.String("node", "", "the node id")
	by := fs.String("by", "the rotation gate", "who issued it")

	if err := fs.Parse(args); err != nil {
		return err
	}

	return unit(ctx, func(b *backupbus.Business, store backupbus.Storer) error {
		m, err := store.MachineByNodeID(ctx, *node)
		if err != nil {
			return err
		}

		plain, _, err := b.IssueCode(ctx, m.ID, *by)
		if err != nil {
			return err
		}

		fmt.Println(plain)

		return nil
	})
}

// show prints every repository row a machine has, newest first.
//
// The gate's window onto what the API does NOT serve. A machine is told about
// one repository and at most one offer; the questions this gate exists to ask
// are about the others — whether the retired row kept its password, whether a
// second bucket landed in `offered` without anybody asking it to, whether the
// old row's release was recorded. None of that is on the wire.
// retire is the administrator closing a rotation: the old bucket becomes
// `retired` and the machine's new one becomes `active`.
//
// The step the machine cannot take for itself, and the gate needs it because
// it is what turns the owner's card from `never` into `superseded` — the
// answer that means "the paper in your safe no longer opens anything", which
// is the whole reason the card endpoint exists.
//
// Two units again, with RetireKeys between them. There are no keys to delete
// here, so the middle is empty — and Eumaeus is told about it honestly by
// SettleRetirement forgetting the key IDs regardless. What is NOT skipped is
// PlanRetirement's refusal to retire a bucket the machine has not released,
// which the gate asserts before it does this properly.
func retire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retire", flag.ExitOnError)
	node := fs.String("node", "", "the node id")
	by := fs.String("by", "the rotation gate", "who is doing this")

	if err := fs.Parse(args); err != nil {
		return err
	}

	return unit(ctx, func(b *backupbus.Business, store backupbus.Storer) error {
		m, err := store.MachineByNodeID(ctx, *node)
		if err != nil {
			return err
		}

		plan, err := b.PlanRetirement(ctx, m.ID, *by)
		if err != nil {
			return err
		}

		repo, err := b.SettleRetirement(ctx, plan)
		if err != nil {
			return err
		}

		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"retired":   repo.URL,
			"state":     string(repo.State),
			"successor": plan.Successor.URL,
		})
	})
}

func show(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	node := fs.String("node", "", "the node id")

	if err := fs.Parse(args); err != nil {
		return err
	}

	return unit(ctx, func(_ *backupbus.Business, store backupbus.Storer) error {
		m, err := store.MachineByNodeID(ctx, *node)
		if err != nil {
			return err
		}

		repos, err := store.RepositoriesOf(ctx, m.ID)
		if err != nil {
			return err
		}

		rows := make([]map[string]any, 0, len(repos))

		for _, r := range repos {
			rows = append(rows, map[string]any{
				"id":             r.ID,
				"url":            r.URL,
				"bucket":         r.Bucket,
				"state":          string(r.State),
				"adopted":        r.Adopted,
				"created_at":     r.CreatedAt,
				"history_since":  r.HistorySince,
				"retired_at":     r.RetiredAt,
				"cutover_at":     r.BeganCutoverAt,
				"release_asked":  r.ReleaseAskedAt,
				"card_issued_at": r.CardIssuedAt,
				"credentials":    r.CredentialsVersion,
			})
		}

		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"machine_id":   m.ID,
			"node_id":      m.NodeID,
			"repositories": rows,
		})
	})
}
