package main

// The tests for seed_cashback.go, driven through run() the way `make
// cashback-seed` drives it: the argv, the environment, and what is printed.
//
// Two halves, and the split matters. Everything this command REFUSES is
// checked without a database, because each refusal exists precisely so that
// nothing is written - a refusal that needed a database to observe would be
// a refusal that had already opened one. What it writes is then checked
// against a real Postgres, in a database of its own, because committing is
// the whole of what the command does.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedBrandDir is the only brand this repository contains, reached from this
// package's directory. There is none it could ship that would not be a lie
// about a real company, which is why the test brand is also what
// `make cashback-seed` defaults to.
const seedBrandDir = "../../internal/platform/brand/testdata/fixture"

// seedEnv is the smallest environment the command needs, over the database
// the caller gives it.
func seedEnv(dbURL string) map[string]string {
	env := cashbackEnv(dbURL)
	env["NETWORK_FIXTURE_ACCOUNT_ID"] = "seed-test-publisher"
	env["NETWORK_FIXTURE_SOURCE_LANGUAGE"] = "en"
	env["BRAND_DIR"] = seedBrandDir
	return env
}

// TestSeedRefusesWhatItCannotSeed. Every case here reaches a refusal before
// a database is opened, which is why the URL below is one nothing could
// connect to: if any of these ever started connecting, this test fails with
// a connection error rather than passing quietly.
func TestSeedRefusesWhatItCannotSeed(t *testing.T) {
	t.Parallel()

	const unreachable = "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable"

	for _, tc := range []struct {
		name  string
		args  []string
		amend func(map[string]string)
		want  string
	}{
		{
			name: "no topic",
			args: []string{"seed"},
			want: "takes a topic",
		},
		{
			name: "a topic it does not seed",
			args: []string{"seed", "news"},
			want: `it seeds "cashback"`,
		},
		{
			name: "more than one account",
			args: []string{"seed", "cashback", uuid.NewString(), uuid.NewString()},
			want: "at most one account id",
		},
		{
			name: "an account id that is not one",
			args: []string{"seed", "cashback", "not-a-uuid"},
			want: "is not an account id",
		},
		{
			// The important one. A seeded route is indistinguishable from
			// an imported one once written, so this refusal is what stands
			// between a developer and rows a real network never agreed to.
			name: "a real network",
			args: []string{"seed", "cashback"},
			amend: func(env map[string]string) {
				env["NETWORKS"] = "awin"
				env["NETWORK_AWIN_ACCOUNT_ID"] = "seed-test-publisher"
				env["NETWORK_AWIN_API_KEY"] = "not-a-real-token"
			},
			want: `refuses to seed against "awin"`,
		},
		{
			name:  "no publisher account",
			args:  []string{"seed", "cashback"},
			amend: func(env map[string]string) { delete(env, "NETWORK_FIXTURE_ACCOUNT_ID") },
			want:  "NETWORK_FIXTURE_ACCOUNT_ID names no publisher account",
		},
		{
			name:  "no source language",
			args:  []string{"seed", "cashback"},
			amend: func(env map[string]string) { delete(env, "NETWORK_FIXTURE_SOURCE_LANGUAGE") },
			want:  "NETWORK_FIXTURE_SOURCE_LANGUAGE is unset",
		},
		{
			name:  "no brand",
			args:  []string{"seed", "cashback"},
			amend: func(env map[string]string) { delete(env, "BRAND_DIR") },
			want:  "BRAND_DIR is unset",
		},
		{
			name:  "a brand that is not there",
			args:  []string{"seed", "cashback"},
			amend: func(env map[string]string) { env["BRAND_DIR"] = "../../internal/platform/brand/testdata/nothing-here" },
			want:  "BRAND_DIR=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := seedEnv(unreachable)
			if tc.amend != nil {
				tc.amend(env)
			}
			var out bytes.Buffer
			err := run(context.Background(), tc.args, func(k string) string { return env[k] }, &out)
			if err == nil {
				t.Fatalf("run(%q) succeeded; it must refuse, and its output was %q", tc.args, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run(%q) = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

// seedCommandDatabase is this command's own scratch database, for the reason
// connect-network has one: committing is what it does, so it cannot run
// inside a rolled-back transaction, and the merchants it imports would
// otherwise collide with the catalogue every other suite in this package
// seeds.
//
// Unlike connect-network's, it is REMADE on every run. This command writes
// rows and then asserts their shape, so a database left standing from last
// time means the second run finds three rate bands already there, takes the
// "already had one" branch, and passes on rows no code in this process
// wrote - the write path silently stops being tested. Coverage is what
// surfaced it: timePtr, reachable only while bands are being written, sat
// at 0.0%.
const seedCommandDatabase = "apivo_seed_cashback_cmd"

var (
	seedDBOnce sync.Once
	seedDBURL  string
	seedDBErr  error
)

// seedTestDB skips unless a database is reachable, and hands back the
// scratch database's URL plus a pool for checking what the command wrote.
func seedTestDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise seed cashback")
	}

	seedDBOnce.Do(func() { seedDBURL, seedDBErr = remakeScratchDatabase(base, seedCommandDatabase) })
	if seedDBErr != nil {
		t.Fatalf("preparing the scratch database: %v", seedDBErr)
	}

	pool, err := pgxpool.New(context.Background(), seedDBURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return seedDBURL, pool
}

// seedOnce runs the command and returns what it printed, failing the test on
// any error: every case below is about what a successful seed leaves behind.
func seedOnce(t *testing.T, dbURL string, args ...string) string {
	t.Helper()
	env := seedEnv(dbURL)
	var out bytes.Buffer
	if err := run(context.Background(), append([]string{"seed", "cashback"}, args...), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatalf("seed cashback %q: %v (output: %s)", args, err, out.String())
	}
	return out.String()
}

// TestSeedWritesWhatAClickOutNeeds. The point of the command is that a
// developer can issue a click-out afterwards, so what is checked is the
// chain a click-out reads: an active network, a route through it, and a band
// whose window is open.
func TestSeedWritesWhatAClickOutNeeds(t *testing.T) {
	dbURL, pool := seedTestDB(t)
	ctx := context.Background()

	out := seedOnce(t, dbURL)

	var active bool
	if err := pool.QueryRow(ctx, `select active from cashback.network where id = 'fixture'`).Scan(&active); err != nil {
		t.Fatalf("reading the network row: %v", err)
	}
	if !active {
		t.Error("the seeded network is inactive, so no click can be issued through it")
	}

	var routes, publishable int
	if err := pool.QueryRow(ctx,
		`select count(*), count(*) filter (where status = 'active')
		   from cashback.merchant_network where network_id = 'fixture'`).Scan(&routes, &publishable); err != nil {
		t.Fatalf("counting routes: %v", err)
	}
	// Three, because the fixture's recorded catalogue holds one retailer in
	// each of the three route states - and the seed imports it rather than
	// inventing its own, so what it writes is what the scheduled job
	// maintains.
	if routes != 3 {
		t.Errorf("seeded %d route(s), want the fixture catalogue's 3", routes)
	}
	if publishable != 1 {
		t.Errorf("%d route(s) are active, want 1: the other two are the suspended and departed retailers", publishable)
	}

	var bands int
	if err := pool.QueryRow(ctx,
		`select count(*) from cashback.offer o
		   join cashback.merchant_network mn on mn.id = o.merchant_network_id
		  where mn.network_id = 'fixture'`).Scan(&bands); err != nil {
		t.Fatalf("counting rate bands: %v", err)
	}
	if bands != 3 {
		t.Errorf("wrote %d rate band(s), want 3: one closed, one live, one not open yet", bands)
	}

	// And exactly one band per publishable route is live, which is what the
	// printed offer id promises the reader they can click through.
	//
	// Asserted PER ROUTE rather than over the network, because a band lives
	// on a route: the day the recording grows a second live retailer the
	// network-wide count becomes two and says nothing, while "one live band
	// on each route" stays the property a click-out depends on (FR-013).
	rows, err := pool.Query(ctx,
		`select mn.external_merchant_id, count(*) filter (
		            where o.valid_from <= now()
		              and coalesce(o.valid_to, 'infinity'::timestamptz) > now())
		   from cashback.merchant_network mn
		   left join cashback.offer o on o.merchant_network_id = mn.id
		  where mn.network_id = 'fixture' and mn.status = 'active'
		  group by mn.external_merchant_id`)
	if err != nil {
		t.Fatalf("counting live bands per route: %v", err)
	}
	defer rows.Close()
	priced := 0
	for rows.Next() {
		var route string
		var live int
		if err := rows.Scan(&route, &live); err != nil {
			t.Fatalf("scanning live bands: %v", err)
		}
		priced++
		if live != 1 {
			t.Errorf("route %s has %d live band(s), want exactly 1: a click-out reads one band and snapshots it (FR-013)", route, live)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("counting live bands per route: %v", err)
	}
	if priced != publishable {
		t.Errorf("%d publishable route(s) were priced, want all %d", priced, publishable)
	}
	if !strings.Contains(out, "live offer      ") {
		t.Errorf("the command printed no live offer id, so a reader has nothing to click through with:\n%s", out)
	}
}

// TestSeedIsSafeToRunAgain. A seed that duplicated its rate bands on every
// run would put two live bands on one route, and a click-out reads one band:
// which one it read would then depend on how many times somebody had seeded.
func TestSeedIsSafeToRunAgain(t *testing.T) {
	dbURL, pool := seedTestDB(t)
	ctx := context.Background()

	seedOnce(t, dbURL)
	var before int
	if err := pool.QueryRow(ctx, `select count(*) from cashback.offer`).Scan(&before); err != nil {
		t.Fatalf("counting rate bands: %v", err)
	}

	out := seedOnce(t, dbURL)
	var after int
	if err := pool.QueryRow(ctx, `select count(*) from cashback.offer`).Scan(&after); err != nil {
		t.Fatalf("counting rate bands: %v", err)
	}
	if after != before {
		t.Errorf("a second run took the rate bands from %d to %d; it must find them rather than write them again", before, after)
	}
	if !strings.Contains(out, "already there") {
		t.Errorf("a second run did not read as a re-run:\n%s", out)
	}
}

// TestSeedOptsAMemberIn, and says plainly when it did not have to.
func TestSeedOptsAMemberIn(t *testing.T) {
	dbURL, pool := seedTestDB(t)
	ctx := context.Background()

	var member uuid.UUID
	if err := pool.QueryRow(ctx,
		`insert into public.account (email, display_name) values ($1, 'Seed Test')
		 returning id`, "seed-"+uuid.NewString()+"@example.com").Scan(&member); err != nil {
		t.Fatalf("creating the member: %v", err)
	}

	if out := seedOnce(t, dbURL, member.String()); !strings.Contains(out, "opted in, terms") {
		t.Errorf("the first run did not opt the member in:\n%s", out)
	}

	var status, brandID string
	if err := pool.QueryRow(ctx,
		`select status, brand_id from cashback.participation where account_id = $1`, member).Scan(&status, &brandID); err != nil {
		t.Fatalf("reading the participation: %v", err)
	}
	if status != "active" {
		t.Errorf("participation status is %q, want active", status)
	}
	if brandID == "" {
		t.Error("the participation names no brand, and brand_id has no default (ADR-0004)")
	}

	// Second run: 0017 freezes the terms on an active row, so the command
	// must report what it found rather than what it would have written.
	out := seedOnce(t, dbURL, member.String())
	if !strings.Contains(out, "was already opted in; nothing changed") {
		t.Errorf("a second run claimed to opt in an already-active member:\n%s", out)
	}
}

// TestSeedRefusesAnAccountItCannotHaveCreated. The account id IS the
// Supabase Auth user id, so this command cannot invent one - and a
// foreign-key violation naming a constraint is not what the person running
// it needs to read.
func TestSeedRefusesAnAccountItCannotHaveCreated(t *testing.T) {
	dbURL, _ := seedTestDB(t)

	env := seedEnv(dbURL)
	var out bytes.Buffer
	stranger := uuid.NewString()
	err := run(context.Background(), []string{"seed", "cashback", stranger},
		func(k string) string { return env[k] }, &out)
	if err == nil {
		t.Fatal("seeding opted in an account that does not exist")
	}
	if !strings.Contains(err.Error(), "no account "+stranger+" exists") {
		t.Errorf("error = %q, want it to name the missing account", err)
	}
	if !strings.Contains(err.Error(), "Supabase Auth user id") {
		t.Errorf("error = %q, want it to say where the id comes from", err)
	}
}
