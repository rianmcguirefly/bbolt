package command

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.etcd.io/bbolt/internal/common"
	"go.etcd.io/bbolt/internal/guts_cli"
)

type freelistInspectOptions struct {
	limit       int
	prefixLen   int
	showKeys    bool
	maxKeysShow int
	sampleSize  int
	noProgress  bool
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
	fs.IntVar(&o.prefixLen, "prefix-len", 16, "key prefix length for grouping")
	fs.BoolVar(&o.showKeys, "show-keys", false, "show sample keys for each prefix")
	fs.IntVar(&o.maxKeysShow, "max-keys", 3, "max sample keys to show per prefix")
	fs.IntVar(&o.sampleSize, "sample", 0, "sample N random pages instead of reading all (0 = read all)")
	fs.BoolVar(&o.noProgress, "no-progress", false, "disable progress indicator")
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
		leafPages   int
		branchPages int
		otherPages  int
		totalKeys   int
		prefixMap   = make(map[string]*prefixStats)
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
			// Page might be corrupted or part of overflow
			otherPages++
			continue
		}

		switch {
		case p.IsLeafPage():
			leafPages++
			// Extract keys from leaf page
			for i := uint16(0); i < p.Count(); i++ {
				elem := p.LeafPageElement(i)
				key := elem.Key()
				totalKeys++

				// Get prefix for grouping
				prefix := truncateKey(key, o.prefixLen)

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

		case p.IsBranchPage():
			branchPages++

		default:
			otherPages++
		}
	}

	// Clear progress line
	if showProgress {
		fmt.Fprintf(stdout, "\r%s\r", strings.Repeat(" ", 50))
	}

	// Print page type breakdown (extrapolate if sampling)
	fmt.Fprintln(stdout, "Page type breakdown:")
	if sampleRatio > 1.0 {
		fmt.Fprintf(stdout, "  Leaf pages:   ~%d (sampled %d)\n", int(float64(leafPages)*sampleRatio), leafPages)
		fmt.Fprintf(stdout, "  Branch pages: ~%d (sampled %d)\n", int(float64(branchPages)*sampleRatio), branchPages)
		if otherPages > 0 {
			fmt.Fprintf(stdout, "  Other/overflow: ~%d (sampled %d)\n", int(float64(otherPages)*sampleRatio), otherPages)
		}
		fmt.Fprintf(stdout, "  Total keys: ~%d (sampled %d)\n\n", int(float64(totalKeys)*sampleRatio), totalKeys)
	} else {
		fmt.Fprintf(stdout, "  Leaf pages:   %d\n", leafPages)
		fmt.Fprintf(stdout, "  Branch pages: %d\n", branchPages)
		if otherPages > 0 {
			fmt.Fprintf(stdout, "  Other/overflow: %d\n", otherPages)
		}
		fmt.Fprintf(stdout, "  Total keys found: %d\n\n", totalKeys)
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

	// Print results
	if sampleRatio > 1.0 {
		fmt.Fprintf(stdout, "Top %d key prefixes in freed pages (prefix-len=%d, extrapolated):\n\n", len(prefixes), o.prefixLen)
		fmt.Fprintf(stdout, "%-*s %10s %10s\n", o.prefixLen+2, "PREFIX", "~KEYS", "~SIZE")
	} else {
		fmt.Fprintf(stdout, "Top %d key prefixes in freed pages (prefix-len=%d):\n\n", len(prefixes), o.prefixLen)
		fmt.Fprintf(stdout, "%-*s %10s %10s\n", o.prefixLen+2, "PREFIX", "KEYS", "SIZE")
	}
	fmt.Fprintf(stdout, "%s\n", strings.Repeat("-", o.prefixLen+2+10+10+2))

	for _, stats := range prefixes {
		displayPrefix := stats.prefix
		if len(displayPrefix) > o.prefixLen {
			displayPrefix = displayPrefix[:o.prefixLen]
		}
		keyCount := stats.keyCount
		byteSize := stats.byteSize
		if sampleRatio > 1.0 {
			keyCount = int(float64(keyCount) * sampleRatio)
			byteSize = int64(float64(byteSize) * sampleRatio)
		}
		fmt.Fprintf(stdout, "%-*s %10d %10s\n",
			o.prefixLen+2,
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

	return nil
}

func truncateKey(key []byte, maxLen int) string {
	if len(key) <= maxLen {
		return bytesToAsciiOrHex(key)
	}
	return bytesToAsciiOrHex(key[:maxLen])
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
