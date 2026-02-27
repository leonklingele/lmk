package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag" //nolint:depguard // We only allow to import the flag package in here
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/jedib0t/go-pretty/v6/table"
	"modernc.org/sqlite"
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

func trimText(t string) string {
	return strings.Trim(t, " \t\r\n")
}

func fixDateString(str string) string {
	str = strings.Split(str, "/")[0]     // 27.03.2025 / 28.03.2025
	str = strings.Split(str, " und ")[0] // 10.06.2025 und 25.06.2025
	str = strings.Split(str, " bis ")[0] // 10.06.2025 bis 25.06.2025
	str = strings.Split(str, ", ")[0]    // 09.12.2025, 10.12.2025, 11.12.2025, 22.12.2025
	return str
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
	var rss []*goquery.Selection
	s.Each(func(_ int, s *goquery.Selection) {
		ss = append(ss, trimText(s.Text()))
		rss = append(rss, s)
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

	// Handle found at with multiple date strings inside
	if n := rss[4].Find(".text p"); n != nil {
		t, err := n.Html()
		if err == nil && strings.Contains(t, ".") {
			foundAtStr = strings.ReplaceAll(t, "<br/>", " / ")
		}
	}

	publishedAtStr = fixDateString(publishedAtStr)
	foundAtStr = fixDateString(foundAtStr)

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

func loadItems(
	ctx context.Context,
	logger *slog.Logger,
	requestTimeout time.Duration,
) ([]*item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lmkURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	res, err := new(http.Client{
		Timeout: requestTimeout,
	}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get: %w", err)
	}
	defer func() {
		err := res.Body.Close()
		if err != nil {
			logger.WarnContext(ctx, "failed to close body", slog.Any("error", err))
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
			e := s.Find(`td`)
			if e.Text() == "Startseite" {
				return true
			}
			itm, err := sel2item(e)
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
	err = <-errch
	if err != nil {
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

func run( //nolint:revive // flag-parameter: bool-option parameters are intentional here
	ctx context.Context,
	logger *slog.Logger,
	sqliteFile string,
	newOnly,
	printAsJSON bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	items, err := loadItems(ctx, logger, requestTimeout)
	if err != nil {
		return err
	}

	//nolint:nestif // Quite complex but somewhat tolerable
	if newOnly {
		var isFirstRun bool
		_, statErr := os.Stat(sqliteFile)
		if errors.Is(statErr, fs.ErrNotExist) {
			isFirstRun = true
		}

		db, err := sql.Open("sqlite", sqliteFile)
		if err != nil {
			return fmt.Errorf("failed to open sqlite database: %w", err)
		}

		if isFirstRun {
			_, err = db.ExecContext(ctx, sqliteInitStmt) //nolint:unqueryvet // const query
			if err != nil {
				return fmt.Errorf("failed to init database: %w", err)
			}
			logger.InfoContext(ctx, "successfully initialized database")
		}

		//nolint:unqueryvet // const query
		stmt, err := db.PrepareContext(ctx, sqliteInsertStmt)
		if err != nil {
			return fmt.Errorf("failed to prepare insert statement: %w", err)
		}
		defer func() {
			err := stmt.Close()
			if err != nil {
				logger.WarnContext(ctx, "failed to close insert statement", slog.Any("error", err))
			}
		}()

		newItems := make([]*item, 0, len(items))
		for _, itm := range items {
			var buf bytes.Buffer
			err = gob.NewEncoder(&buf).Encode(itm)
			if err != nil {
				return fmt.Errorf("failed to gob-encode item %+v: %w", itm, err)
			}

			hash := sha256.Sum256(buf.Bytes())

			_, err = stmt.ExecContext(
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
			)
			if err != nil {
				// Allow "UNIQUE constraint" errors.
				// Error code taken from https://www.sqlite.org/rescode.html#constraint_unique
				serr, ok := errors.AsType[*sqlite.Error](err)
				if ok && serr.Code() == 2067 {
					// This is fine
					continue
				}

				logger.ErrorContext(ctx,
					"failed to exec insert statement",
					slog.Any("err", err),
					slog.Any("item", itm),
				)
				continue
			}

			newItems = append(newItems, itm)
		}

		items = newItems
	}

	//nolint:nestif // Two distinct output modes with their own internal logic
	if printAsJSON {
		// Print as JSON
		enc := json.NewEncoder(os.Stdout)
		for _, itm := range items {
			err := enc.Encode(itm)
			if err != nil {
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

		//nolint:errcheck,forbidigo // We explicitly want to print to stdout
		fmt.Println(t.Render())
	}

	return nil
}

func getenv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func main() {
	// Env vars
	var (
		logLevel = getenv("LOG_LEVEL", slog.LevelInfo.String())

		sqliteFile = getenv("SQLITE_FILE", defaultSQLiteFilePath)
	)

	// Flags
	var (
		newOnly     = flag.Bool("new", false, "new items only")
		printAsJSON = flag.Bool("json", false, "print as JSON")
	)
	flag.Parse()

	var ll slog.LevelVar
	err := ll.UnmarshalText([]byte(logLevel))
	if err != nil {
		//nolint:forbidigo // Fine to panic in main
		panic(fmt.Errorf("unsupported log level: %s", logLevel))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     &ll,
		AddSource: true,
	}))
	slog.SetDefault(logger)

	ctx, sstop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer sstop()

	err = run(
		ctx,
		logger,
		sqliteFile,
		*newOnly,
		*printAsJSON,
	)
	if err != nil {
		logger.ErrorContext(ctx, "failed to run", slog.Any("error", err))
		os.Exit(1) //nolint:gocritic // Fine to not run deferred sstop
	}
}
