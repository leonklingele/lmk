package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"flag" //nolint:depguard // We only allow to import the flag package in here
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/jedib0t/go-pretty/v6/table"

	_ "modernc.org/sqlite"
)

const (
	tableMaxWidth    = 42
	tableShowDetails = true

	timeFormat = "02.01.2006"

	requestTimeout = 10 * time.Second

	lmkURL = "https://verbraucherinfo-bw.de/,Lde/Startseite/Lebensmittelkontrolle"
)

const (
	defaultSQLiteFilePath = "./db.sqlite"

	sqliteInitStmt = `
		begin;
		create table items (
			id integer primary key not null,
			hash text unique not null,
			authority text not null,
			published_at text not null,
			found_at text not null,
			name text not null,
			address text not null,
			reason text not null,
			legal_basis text not null,
			info text not null
		) strict;
		commit;
	`
	sqliteInsertStmt = `
		insert into items (
			hash,
			authority,
			published_at,
			found_at,
			name,
			address,
			reason,
			legal_basis,
			info
		) values (
			?, ?, ?, ?, ?, ?, ?, ?, ?
		);
	`
)

//nolint:gochecknoglobals // Nice to use as a global
var logTarget = os.Stderr

func trimText(t string) string {
	return strings.Trim(t, " \t\r\n")
}

type item struct {
	Authority      string    `json:"authority"`
	PublishedAt    time.Time `json:"published_at"`
	PublishedAtStr string    `json:"-"`
	FoundAt        time.Time `json:"found_at"`
	FoundAtStr     string    `json:"-"`
	Name           string    `json:"name"`
	Address        string    `json:"address"`
	Reason         string    `json:"reason"`
	LegalBasis     string    `json:"legal_basis"`
	Info           string    `json:"info"`
}

func sel2item(s *goquery.Selection) (*item, error) {
	var ss []string
	s.Each(func(_ int, s *goquery.Selection) {
		ss = append(ss, trimText(s.Text()))
	})

	// Generally, we expect 8 columns. However, for some rows, the last column (info) is missing, so we add an empty string
	if got, want := len(ss), 8; got != want {
		if got != want-1 {
			details, err := s.Html()
			if err != nil {
				details = err.Error()
			}
			return nil, fmt.Errorf("invalid number of parts found %d/%d: %s", got, want, details)
		}

		ss = append(ss, "") // Add empty string for missing info
	}

	for i, s := range ss {
		ss[i] = trimText(s)
	}

	authority,
		publishedAtStr,
		name,
		address,
		foundAtStr,
		reason,
		legalBasis,
		info := ss[0],
		ss[1],
		ss[2],
		ss[3],
		ss[4],
		ss[5],
		ss[6],
		ss[7]

	publishedAtStr = strings.Split(publishedAtStr, "/")[0]     // 27.03.2025 / 28.03.2025
	publishedAtStr = strings.Split(publishedAtStr, " und ")[0] // 10.06.2025 und 25.06.2025
	publishedAtStr = strings.Split(publishedAtStr, " bis ")[0] // 10.06.2025 bis 25.06.2025

	foundAtStr = strings.TrimSuffix(foundAtStr, "z")   // Theres one item with a trailing "z"
	foundAtStr = strings.Split(foundAtStr, "/")[0]     // 27.03.2025 / 28.03.2025
	foundAtStr = strings.Split(foundAtStr, " und ")[0] // 10.06.2025 und 25.06.2025
	foundAtStr = strings.Split(foundAtStr, " bis ")[0] // 10.06.2025 bis 25.06.2025

	itm := &item{
		Authority:      authority,
		PublishedAtStr: publishedAtStr,
		FoundAtStr:     foundAtStr,
		Name:           name,
		Address:        address,
		Reason:         reason,
		LegalBasis:     legalBasis,
		Info:           info,
	}

	publishedAtStr = trimText(publishedAtStr)
	if strings.Contains(publishedAtStr, ".") { // Looks like a date
		publishedAt, err := time.Parse(timeFormat, publishedAtStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse published at %q: %w", publishedAtStr, err)
		}
		itm.PublishedAt = publishedAt
	}

	foundAtStr = trimText(foundAtStr)
	if strings.Contains(foundAtStr, ".") { // Looks like a date
		foundAt, err := time.Parse(timeFormat, foundAtStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse found at %q: %w", foundAtStr, err)
		}
		itm.FoundAt = foundAt
	}

	return itm, nil
}

func loadItems(ctx context.Context, requestTimeout time.Duration, l *slog.Logger) ([]*item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lmkURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	res, err := (&http.Client{
		Timeout: requestTimeout,
	}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			l.ErrorContext(ctx, fmt.Errorf("failed to close body: %w", err).Error())
		}
	}()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to create document: %w", err)
	}

	tbl := doc.Find(`#consumerInfoTable`)

	// Sanity check
	hl, err := sel2item(tbl.Find(`thead th p`))
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve table heading: %w", err)
	}
	if hl.Authority != "Behörde" ||
		hl.PublishedAtStr != "Datum Veröffentlichung" ||
		hl.FoundAtStr != "Feststellungstag" ||
		hl.Name != "Betriebsbezeichnung" ||
		hl.Address != "Anschrift" ||
		hl.Reason != "Sachverhalt/Grund der Beanstandung" ||
		hl.LegalBasis != "Rechtsgrundlage" ||
		hl.Info != "Hinweise zur Mängelbeseitigung und Bemerkungen" {
		return nil, fmt.Errorf("labels incorrect, has the page design changed? %+v", hl)
	}

	var items []*item
	errch := make(chan error, 1)
	tbl.
		Find(`tbody tr`).
		EachWithBreak(func(_ int, s *goquery.Selection) bool {
			itm, err := sel2item(s.Find(`td`))
			if err != nil {
				details, err2 := s.Html()
				if err2 != nil {
					details = err2.Error()
				}
				errch <- fmt.Errorf("failed to retrieve item from selection %s: %w", details, err)
				return false
			}

			items = append(items, itm)
			return true
		})
	close(errch)
	if err := <-errch; err != nil {
		return nil, err
	}

	// Order by published at
	slices.SortStableFunc(items, func(a, b *item) int {
		return a.PublishedAt.Compare(b.PublishedAt)
	})

	return items, nil
}

func capstring(s string, l int) string { //nolint:unparam // False positive
	if len(s) <= l {
		return s
	}
	return s[:l] + "…"
}

func run( //nolint:revive // They are bool-options
	ctx context.Context,
	l *slog.Logger,
	sqliteFile string,
	newOnly,
	printAsJSON bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	items, err := loadItems(ctx, requestTimeout, l)
	if err != nil {
		return err
	}

	//nolint:nestif // Quite complex but somewhat tolerable
	if newOnly {
		var isFirstRun bool
		if _, err := os.Stat(sqliteFile); os.IsNotExist(err) {
			isFirstRun = true
		}

		db, err := sql.Open("sqlite", sqliteFile)
		if err != nil {
			return fmt.Errorf("failed to open sqlite database: %w", err)
		}

		if isFirstRun {
			if _, err := db.ExecContext(ctx, sqliteInitStmt); err != nil {
				return fmt.Errorf("failed to init database: %w", err)
			}
			l.InfoContext(ctx, "successfully initialized database")
		}

		stmt, err := db.PrepareContext(ctx, sqliteInsertStmt)
		if err != nil {
			return fmt.Errorf("failed to prepare insert statement: %w", err)
		}
		defer func() {
			if err := stmt.Close(); err != nil {
				l.ErrorContext(ctx, fmt.Errorf("failed to close insert statement: %w", err).Error())
			}
		}()

		newItems := make([]*item, 0, len(items))
		for _, itm := range items {
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(itm); err != nil {
				return fmt.Errorf("failed to gob-encode item %+v: %w", itm, err)
			}

			hash := sha256.Sum256(buf.Bytes())

			if _, err := stmt.ExecContext(
				ctx,
				hex.EncodeToString(hash[:]),
				itm.Authority,
				itm.PublishedAt,
				itm.FoundAt,
				itm.Name,
				itm.Address,
				itm.Reason,
				itm.LegalBasis,
				itm.Info,
			); err != nil {
				// TODO: Properly check for error, see https://gitlab.com/cznic/sqlite/-/blob/f49aba7eddcec7d31797e72c67aafb0398970730/all_test.go#L2228
				if got, want := err.Error(), "constraint failed: UNIQUE constraint failed: items.hash (2067)"; got == want {
					// This is fine
					continue
				}

				l.ErrorContext(
					ctx,
					"failed to exec insert statement",
					"err", err,
					"item", fmt.Sprintf("%+v", itm),
				)
				continue
			}

			newItems = append(newItems, itm)
		}

		items = newItems
	}

	//nolint:nestif // Quite complex but somewhat tolerable
	if printAsJSON {
		// Print as JSON
		enc := json.NewEncoder(os.Stdout)
		for _, itm := range items {
			if err := enc.Encode(itm); err != nil {
				return fmt.Errorf("failed to JSON-print to stdout: %w", err)
			}
		}
	} else {
		// Print as table
		t := table.NewWriter()
		t.SetAutoIndex(true)
		t.SetTitle("Lebensmittelkontrolle")
		header := table.Row{
			"Behörde",
			"Datum Veröffentlichung",
			"Feststellungstag",
			"Betriebsbezeichnung",
			"Anschrift",
		}
		if tableShowDetails {
			for _, h := range []string{
				"Sachverhalt/Grund der Beanstandung",
				"Rechtsgrundlage",
				"Hinweise zur Mängelbeseitigung und Bemerkungen",
			} {
				header = append(header, h)
			}
		}
		t.AppendHeader(header)
		for _, itm := range items {
			row := table.Row{
				capstring(itm.Authority, tableMaxWidth),
				capstring(itm.PublishedAtStr, tableMaxWidth),
				capstring(itm.FoundAtStr, tableMaxWidth),
				capstring(itm.Name, tableMaxWidth),
				capstring(itm.Address, tableMaxWidth),
			}
			if tableShowDetails {
				for _, r := range []string{
					capstring(itm.Reason, tableMaxWidth),
					capstring(itm.LegalBasis, tableMaxWidth),
					capstring(itm.Info, tableMaxWidth),
				} {
					row = append(row, r)
				}
			}
			t.AppendRow(row)
		}

		//nolint:forbidigo // We explicitly want to print to stdout
		if _, err := fmt.Println(t.Render()); err != nil {
			return fmt.Errorf("failed to print to stdout: %w", err)
		}
	}

	return nil
}

func main() {
	newOnly := flag.Bool("new", false, "new items only")
	printAsJSON := flag.Bool("json", false, "print as JSON")

	debug := flag.Bool("debug", false, "enable debug mode")

	flag.Parse()

	sqliteFile := getenv("SQLITE_FILE", defaultSQLiteFilePath)

	ll := new(slog.LevelVar)
	ll.Set(slog.LevelInfo)
	l := slog.New(slog.NewJSONHandler(logTarget, &slog.HandlerOptions{
		Level: ll,
	}))
	slog.SetDefault(l)

	// We have a debug env var as well as a debug CLI flag
	if getenv("DEBUG", "false") == "true" {
		*debug = true
	}

	if *debug {
		ll.Set(slog.LevelDebug)
	}

	ctx := context.Background()

	if err := run(
		ctx,
		l,
		sqliteFile,
		*newOnly,
		*printAsJSON,
	); err != nil {
		l.ErrorContext(ctx, err.Error())
	}
}
