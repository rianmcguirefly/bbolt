package command_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	bolt "go.etcd.io/bbolt"
	"go.etcd.io/bbolt/cmd/bbolt/command"
	"go.etcd.io/bbolt/internal/btesting"
)

func TestFreelistInspect(t *testing.T) {
	db := btesting.MustCreateDB(t)
	defer db.Close()

	// Create bucket and add data
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("testbucket"))
		if err != nil {
			return err
		}
		// Add many keys
		for i := 0; i < 1000; i++ {
			key := []byte(strings.Repeat("k", 100) + string(rune(i)))
			val := []byte(strings.Repeat("v", 500))
			if err := b.Put(key, val); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	// Delete the data to create free pages
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte("testbucket"))
	})
	require.NoError(t, err)

	// Close and reopen to ensure freelist is synced
	db.MustClose()
	db.MustReopen()
	defer db.Close()

	// Verify there are free pages
	stats := db.Stats()
	require.Greater(t, stats.FreePageN, 0, "expected free pages after deletion")

	// Run the freelist-inspect command
	rootCmd := command.NewRootCommand()
	rootCmd.SetArgs([]string{"freelist-inspect", db.Path()})

	var out strings.Builder
	rootCmd.SetOut(&out)

	err = rootCmd.Execute()
	require.NoError(t, err)

	output := out.String()
	t.Log(output)

	// Verify output contains expected elements
	require.Contains(t, output, "Free pages:")
	require.Contains(t, output, "Leaf pages:")
	require.Contains(t, output, "PREFIX")
}

func TestFreelistInspectEmptyFreelist(t *testing.T) {
	db := btesting.MustCreateDB(t)
	defer db.Close()

	// Create a bucket but don't delete anything
	err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte("testbucket"))
		return err
	})
	require.NoError(t, err)

	rootCmd := command.NewRootCommand()
	rootCmd.SetArgs([]string{"freelist-inspect", db.Path()})

	var out strings.Builder
	rootCmd.SetOut(&out)

	err = rootCmd.Execute()
	require.NoError(t, err)

	output := out.String()
	t.Log(output)
	// Should indicate no meaningful keys in freelist
	require.Contains(t, output, "No keys found in freed pages")
}
