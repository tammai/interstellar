// Command watchlist imports a watchlist CSV into an Interstellar database.
//
// A command rather than an endpoint for now: the import is additive and
// idempotent, so it is safe to re-run, and there is nothing on a screen that
// would make the conflict list easier to act on than a terminal does.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/tammai/interstellar/internal/store"
)

func main() {
	dbPath := flag.String("db", "./.devdata/interstellar.db", "path to interstellar.db")
	file := flag.String("file", "", "watchlist CSV")
	flag.Parse()

	if *file == "" {
		log.Fatal("--file is required")
	}
	f, err := os.Open(*file)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	rows, err := store.ParseWatchlistCSV(f)
	if err != nil {
		log.Fatal(err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	res, err := db.ImportWatchlist(rows)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("read %d rows: %d added, %d updated, %d aliases, %d skipped\n",
		len(rows), res.Added, res.Updated, res.AliasesAdded, res.Skipped)
	for _, c := range res.Conflicts {
		fmt.Println("  conflict:", c)
	}
}
