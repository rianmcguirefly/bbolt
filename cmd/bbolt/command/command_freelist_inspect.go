package command

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.etcd.io/bbolt/internal/common"
	"go.etcd.io/bbolt/internal/guts_cli"
)

type freelistInspectOptions struct {
	limit        int
	separator    string
	segments     int
	showKeys     bool
	sampleSize   int
	noProgress   bool
	dumpOverflow string
}

func newFreelistInspectCommand() *cobra.Command {
	var o freelistInspectOptions
	cmd := &cobra.Command{
		Use:   "freelist-inspect <bbolt-file>",
		Short: "analyze contents of freed pages to show what compaction would remove",
		Long: strings.TrimLeft(`
Reads all pages in the freelist and inspects their contents.
Since freed pages aren't zeroed, we can see what data was deleted.

This helps answer: "What data is sitting in freed pages that
compaction would reclaim?"

For leaf pages, keys are extracted and grouped by prefix to help
identify which buckets the deleted data came from.
`, "\n"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.Run(cmd, args[0])
		},
	}
	o.AddFlags(cmd.Flags())
	return cmd
}

func (o *freelistInspectOptions) AddFlags(fs *pflag.FlagSet) {
	fs.IntVarP(&o.limit, "limit", "n", 20, "number of key prefixes to show")
	fs.StringVar(&o.separator, "sep", "#", "separator character for key segments")
	fs.IntVar(&o.segments, "segments", 2, "number of segments to include in prefix")
	fs.BoolVar(&o.showKeys, "show-keys", false, "show sample keys for each prefix")
	fs.IntVar(&o.sampleSize, "sample", 0, "sample N random pages instead of reading all (0 = read all)")
	fs.BoolVar(&o.noProgress, "no-progress", false, "disable progress indicator")
	fs.StringVar(&o.dumpOverflow, "dump-overflow", "", "dump unattributed overflow page IDs to file")
}

type prefixStats struct {
	prefix    string
	keyCount  int
	byteSize  int64
	sampleKey string
}

func (o *freelistInspectOptions) Run(cmd *cobra.Command, dbPath string) error {
	if _, err := checkSourceDBPath(dbPath); err != nil {
		return err
	}

	// Get page size and active meta
	pageSize, _, err := guts_cli.ReadPageAndHWMSize(dbPath)
	if err != nil {
		return fmt.Errorf("failed to read page size: %w", err)
	}

	meta, _, err := guts_cli.GetActiveMetaPage(dbPath)
	if err != nil {
		return fmt.Errorf("failed to read meta page: %w", err)
	}

	freelistPgid := meta.Freelist()
	if freelistPgid == common.PgidNoFreelist {
		fmt.Fprintln(cmd.OutOrStdout(), "Database has no synced freelist (NoFreelistSync mode)")
		fmt.Fprintln(cmd.OutOrStdout(), "Cannot inspect freed pages without freelist")
		return nil
	}

	// Read freelist page
	freelistPage, _, err := guts_cli.ReadPage(dbPath, uint64(freelistPgid))
	if err != nil {
		return fmt.Errorf("failed to read freelist page %d: %w", freelistPgid, err)
	}

	freePageIDs := freelistPage.FreelistPageIds()
	if len(freePageIDs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Freelist is empty - nothing to compact")
		return nil
	}

	stdout := cmd.OutOrStdout()
	fmt.Fprintf(stdout, "Database: %s\n", dbPath)
	fmt.Fprintf(stdout, "Page size: %d bytes\n", pageSize)
	fmt.Fprintf(stdout, "Free pages: %d (%s total)\n",
		len(freePageIDs), formatSize(len(freePageIDs)*int(pageSize)))

	// Sample pages if requested
	pagesToScan := freePageIDs
	sampleRatio := 1.0
	if o.sampleSize > 0 && o.sampleSize < len(freePageIDs) {
		// Shuffle and take first N
		shuffled := make([]common.Pgid, len(freePageIDs))
		copy(shuffled, freePageIDs)
		rand.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		pagesToScan = shuffled[:o.sampleSize]
		sampleRatio = float64(len(freePageIDs)) / float64(o.sampleSize)
		fmt.Fprintf(stdout, "Sampling: %d of %d pages (%.1fx extrapolation)\n",
			o.sampleSize, len(freePageIDs), sampleRatio)
	}
	fmt.Fprintln(stdout)

	// Analyze each free page
	var (
		leafPages         int
		branchPages       int
		freelistPages     int
		overflowPages     int
		unattributed      int
		totalKeys         int
		prefixMap         = make(map[string]*prefixStats)
		unattributedPages []common.Pgid
	)

	totalToScan := len(pagesToScan)
	progressInterval := totalToScan / 100
	if progressInterval < 1 {
		progressInterval = 1
	}
	showProgress := !o.noProgress && totalToScan > 1000

	for i, pgid := range pagesToScan {
		if showProgress && i%progressInterval == 0 {
			pct := float64(i) * 100 / float64(totalToScan)
			fmt.Fprintf(stdout, "\rScanning pages: %.0f%% (%d/%d)", pct, i, totalToScan)
		}
		p, _, err := guts_cli.ReadPage(dbPath, uint64(pgid))
		if err != nil {
			// Page might be corrupted
			unattributed++
			continue
		}

		switch {
		case p.IsLeafPage():
			leafPages++
			// Extract keys from leaf page
			o.processLeafPage(p, prefixMap, &totalKeys)

		case p.IsBranchPage():
			branchPages++

		case p.IsFreelistPage():
			freelistPages++

		default:
			// Try to find parent leaf page for overflow pages
			parentFound := false

			// Scan backwards to find parent leaf page (up to 100000 pages back)
			for back := uint64(1); back <= 100000 && uint64(pgid) >= back; back++ {
				parentID := uint64(pgid) - back
				parentPage, _, err := guts_cli.ReadPage(dbPath, parentID)
				if err != nil {
					continue
				}

				// Check if this is a leaf page that covers our overflow page
				if parentPage.IsLeafPage() && uint64(parentPage.Overflow()) >= back {
					// Found the parent - attribute overflow size to its keys
					overflowPages++
					parentFound = true

					// Attribute to the last key (usually the large one causing overflow)
					if parentPage.Count() > 0 {
						elem := parentPage.LeafPageElement(parentPage.Count() - 1)
						key := elem.Key()
						prefix := extractPrefix(key, o.separator, o.segments)

						stats, exists := prefixMap[prefix]
						if !exists {
							stats = &prefixStats{prefix: prefix}
							prefixMap[prefix] = stats
						}
						// Add one page worth of size for this overflow page
						stats.byteSize += int64(pageSize)
						if stats.sampleKey == "" {
							stats.sampleKey = bytesToAsciiOrHex(key)
						}
					}
					break
				}
			}

			if !parentFound {
				unattributed++
				if o.dumpOverflow != "" {
					unattributedPages = append(unattributedPages, pgid)
				}
			}
		}
	}

	// Clear progress line
	if showProgress {
		fmt.Fprintf(stdout, "\r%s\r", strings.Repeat(" ", 50))
	}

	// Print page type breakdown (extrapolate if sampling)
	fmt.Fprintln(stdout, "Page type breakdown:")
	if sampleRatio > 1.0 {
		fmt.Fprintf(stdout, "  Leaf pages:            ~%d (sampled %d)\n", int(float64(leafPages)*sampleRatio), leafPages)
		fmt.Fprintf(stdout, "  Branch pages:          ~%d (sampled %d)\n", int(float64(branchPages)*sampleRatio), branchPages)
		fmt.Fprintf(stdout, "  Old freelist pages:    ~%d (sampled %d)\n", int(float64(freelistPages)*sampleRatio), freelistPages)
		if overflowPages > 0 {
			fmt.Fprintf(stdout, "  Overflow (attributed): ~%d (sampled %d)\n", int(float64(overflowPages)*sampleRatio), overflowPages)
		}
		if unattributed > 0 {
			fmt.Fprintf(stdout, "  Overflow (unknown):    ~%d (sampled %d)\n", int(float64(unattributed)*sampleRatio), unattributed)
		}
		fmt.Fprintf(stdout, "  Total keys:            ~%d (sampled %d)\n\n", int(float64(totalKeys)*sampleRatio), totalKeys)
	} else {
		fmt.Fprintf(stdout, "  Leaf pages:            %d\n", leafPages)
		fmt.Fprintf(stdout, "  Branch pages:          %d\n", branchPages)
		fmt.Fprintf(stdout, "  Old freelist pages:    %d\n", freelistPages)
		if overflowPages > 0 {
			fmt.Fprintf(stdout, "  Overflow (attributed): %d\n", overflowPages)
		}
		if unattributed > 0 {
			fmt.Fprintf(stdout, "  Overflow (unknown):    %d\n", unattributed)
		}
		fmt.Fprintf(stdout, "  Total keys found:      %d\n\n", totalKeys)
	}

	if len(prefixMap) == 0 {
		fmt.Fprintln(stdout, "No keys found in freed pages")
		return nil
	}

	// Sort by key count (descending)
	prefixes := make([]*prefixStats, 0, len(prefixMap))
	for _, stats := range prefixMap {
		prefixes = append(prefixes, stats)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return prefixes[i].keyCount > prefixes[j].keyCount
	})

	// Limit output
	if len(prefixes) > o.limit {
		prefixes = prefixes[:o.limit]
	}

	// Calculate max prefix width for formatting
	maxPrefixWidth := 20
	for _, stats := range prefixes {
		if len(stats.prefix) > maxPrefixWidth {
			maxPrefixWidth = len(stats.prefix)
		}
	}
	if maxPrefixWidth > 60 {
		maxPrefixWidth = 60
	}

	// Print results
	if sampleRatio > 1.0 {
		fmt.Fprintf(stdout, "Top %d key prefixes in freed pages (sep=%q, segments=%d, extrapolated):\n\n",
			len(prefixes), o.separator, o.segments)
		fmt.Fprintf(stdout, "%-*s %10s %10s\n", maxPrefixWidth, "PREFIX", "~KEYS", "~SIZE")
	} else {
		fmt.Fprintf(stdout, "Top %d key prefixes in freed pages (sep=%q, segments=%d):\n\n",
			len(prefixes), o.separator, o.segments)
		fmt.Fprintf(stdout, "%-*s %10s %10s\n", maxPrefixWidth, "PREFIX", "KEYS", "SIZE")
	}
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", maxPrefixWidth+10+10+2))

	for _, stats := range prefixes {
		displayPrefix := stats.prefix
		if len(displayPrefix) > maxPrefixWidth {
			displayPrefix = displayPrefix[:maxPrefixWidth-3] + "..."
		}
		keyCount := stats.keyCount
		byteSize := stats.byteSize
		if sampleRatio > 1.0 {
			keyCount = int(float64(keyCount) * sampleRatio)
			byteSize = int64(float64(byteSize) * sampleRatio)
		}
		fmt.Fprintf(stdout, "%-*s %10d %10s\n",
			maxPrefixWidth,
			displayPrefix,
			keyCount,
			formatSize(int(byteSize)),
		)
		if o.showKeys && stats.sampleKey != "" {
			fmt.Fprintf(stdout, "  sample: %s\n", truncateString(stats.sampleKey, 60))
		}
	}

	if len(prefixMap) > o.limit {
		fmt.Fprintf(stdout, "\n... and %d more prefixes\n", len(prefixMap)-o.limit)
	}

	// Dump unattributed overflow pages if requested
	if o.dumpOverflow != "" && len(unattributedPages) > 0 {
		f, err := os.Create(o.dumpOverflow)
		if err != nil {
			return fmt.Errorf("failed to create dump file: %w", err)
		}
		defer f.Close()

		for _, pgid := range unattributedPages {
			fmt.Fprintf(f, "%d\n", pgid)
		}
		fmt.Fprintf(stdout, "\nDumped %d unattributed overflow page IDs to %s\n", len(unattributedPages), o.dumpOverflow)
	}

	return nil
}

func (o *freelistInspectOptions) processLeafPage(p *common.Page, prefixMap map[string]*prefixStats, totalKeys *int) {
	for i := uint16(0); i < p.Count(); i++ {
		elem := p.LeafPageElement(i)
		key := elem.Key()
		*totalKeys++

		prefix := extractPrefix(key, o.separator, o.segments)

		stats, exists := prefixMap[prefix]
		if !exists {
			stats = &prefixStats{prefix: prefix}
			prefixMap[prefix] = stats
		}
		stats.keyCount++
		stats.byteSize += int64(elem.Ksize() + elem.Vsize())
		if stats.sampleKey == "" {
			stats.sampleKey = bytesToAsciiOrHex(key)
		}
	}
}

func extractPrefix(key []byte, sep string, segments int) string {
	s := bytesToAsciiOrHex(key)
	if sep == "" || segments <= 0 {
		return s
	}

	count := 0
	for i := 0; i < len(s); i++ {
		if strings.HasPrefix(s[i:], sep) {
			count++
			if count >= segments {
				return s[:i]
			}
		}
	}
	// Fewer segments than requested, return whole key
	return s
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
