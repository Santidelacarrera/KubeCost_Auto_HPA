package psi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAndReadCgroupPressure(t *testing.T) {
	dir := t.TempDir()
	cpuContent := "some avg10=18.50 avg60=6.20 avg300=2.10 total=849201\nfull avg10=1.20 avg60=0.40 avg300=0.10 total=41200\n"
	memContent := "some avg10=24.10 avg60=11.00 avg300=4.00 total=910293\nfull avg10=7.80 avg60=2.10 avg300=0.50 total=192831\n"

	if err := os.WriteFile(filepath.Join(dir, "cpu.pressure"), []byte(cpuContent), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.pressure"), []byte(memContent), 0600); err != nil {
		t.Fatal(err)
	}

	snap, err := ReadCgroupPressure(dir)
	if err != nil {
		t.Fatalf("unexpected error reading PSI: %v", err)
	}
	if !snap.CPUThrottlingSpike {
		t.Fatalf("expected CPUThrottlingSpike=true for avg10=18.50, got %+v", snap)
	}
	if !snap.MemReclaimDistress {
		t.Fatalf("expected MemReclaimDistress=true for full.avg10=7.80, got %+v", snap)
	}
	if snap.CPUSome.Total != 849201 || snap.MemFull.Total != 192831 {
		t.Fatalf("unexpected total counters: %+v", snap)
	}
}
