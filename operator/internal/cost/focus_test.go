package cost

// SYNTHETIC fixtures that follow the FOCUS column names, except testdata/gcp-native-focus-converted.csv (a real GCP export row).

import (
	"os"
	"strings"
	"testing"
	"time"
)

const focusOK = `BillingCurrency,ChargeCategory,ChargePeriodStart,ChargePeriodEnd,BilledCost,ResourceId,ServiceName
USD,Usage,2026-11-01T00:00:00Z,2026-11-03T00:00:00Z,48.00,arn:aws:ec2:us-east-1:123456789012:instance/i-0abc,Amazon EC2
USD,Usage,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,6.00,vol-0def,Amazon EBS
USD,Credit,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,-5.00,i-0abc,Amazon EC2
USD,Tax,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,2.50,,Tax
USD,Usage,2026-10-01T00:00:00Z,2026-10-02T00:00:00Z,99.00,i-old,Amazon EC2
`

func TestFOCUSUsageIsProratedAndOtherChargesAreReportedNotAllocated(t *testing.T) {
	items, sum, err := ParseFOCUS(strings.NewReader(focusOK), t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Cost != usd("24") || items[1].Cost != usd("6") {
		t.Fatalf("%+v", items)
	}
	if sum.Skipped["Credit"] != -usd("5") || sum.Skipped["Tax"] != usd("2.5") || sum.Rows != 5 {
		t.Fatalf("%+v", sum)
	}
	if !strings.Contains(strings.Join(sum.Notes, " "), "NOT allocated") {
		t.Fatalf("%v", sum.Notes)
	}
}

func TestFOCUSResourceIdsFindNodesByProviderIDAndUnmatchedBecomeUnallocated(t *testing.T) {
	items, _, _ := ParseFOCUS(strings.NewReader(focusOK), t0, t1)
	nodes := []Node{
		{Name: "ip-10-0-1-5", ProviderID: "aws:///us-east-1a/i-0abc", CPUAllocatable: 4, MemAllocatable: 16 << 30},
		{Name: "other", ProviderID: "aws:///us-east-1b/i-0zzz", CPUAllocatable: 4, MemAllocatable: 16 << 30},
	}
	nc, extra := SplitByNodeIdentity(items, nodes)
	if nc["ip-10-0-1-5"] != usd("24") || len(nc) != 1 || len(extra) != 1 || extra[0].Resource != "vol-0def" {
		t.Fatalf("%v %v", nc, extra)
	}
	r, err := Allocate(BasisBilled, "synthetic-focus", t0, t1, nodes, nil, nc, extra, Config{})
	if err != nil || !r.Reconciled || r.ByNamespace[Unallocated] != usd("6") || r.Total != usd("30") {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestProviderIDShapesOfTheThreeClouds(t *testing.T) {
	nodes := []Node{
		{Name: "gke-1", ProviderID: "gce://my-project/us-central1-a/gke-1"},
		{Name: "aks-1", ProviderID: "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"},
		{Name: "vm-named", ProviderID: "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/VM-Named"},
	}
	items := []BillingItem{
		{Resource: "//compute.googleapis.com/projects/my-project/zones/us-central1-a/instances/gke-1", Cost: usd("1")},
		{Resource: "/subscriptions/s/resourcegroups/rg/providers/microsoft.compute/virtualmachines/vm-named", Cost: usd("2")},
	}
	nc, extra := SplitByNodeIdentity(items, nodes)
	if nc["gke-1"] != usd("1") || nc["vm-named"] != usd("2") || len(extra) != 0 {
		t.Fatalf("%v %v", nc, extra)
	}
}

func TestFOCUSRejectsWhatItCannotInterpretHonestly(t *testing.T) {
	hdr := "BillingCurrency,ChargeCategory,ChargePeriodStart,ChargePeriodEnd,BilledCost,ResourceId\n"
	for name, in := range map[string]string{
		"empty":        "",
		"no column":    "BilledCost,ResourceId\n1,x\n",
		"euro":         hdr + "EUR,Usage,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,5,x\n",
		"bad cost":     hdr + "USD,Usage,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,abc,x\n",
		"negative use": hdr + "USD,Usage,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z,-5,x\n",
		"bad time":     hdr + "USD,Usage,soon,2026-11-02T00:00:00Z,5,x\n",
		"inverted":     hdr + "USD,Usage,2026-11-02T00:00:00Z,2026-11-01T00:00:00Z,5,x\n",
	} {
		if _, _, err := ParseFOCUS(strings.NewReader(in), t0, t1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	items, sum, err := ParseFOCUS(strings.NewReader("BilledCost,ResourceId,ChargePeriodStart,ChargePeriodEnd\n3,x,2026-11-01T00:00:00Z,2026-11-02T00:00:00Z\n"), t0, t0.Add(24*time.Hour))
	if err != nil || len(items) != 1 || !strings.Contains(strings.Join(sum.Notes, " "), "ASSUMED to be USD") {
		t.Fatalf("a missing currency column must be flagged, not guessed silently: %v %+v", err, sum)
	}
}

// testdata/gcp-native-focus-converted.csv is the REAL output (2026-11-10, CRLF line ends as bq writes them) of the native-FOCUS
// query in hack/gke-soak.sh on Google's FOCUS export for an account billed in CLP: the per-row conversion to USD and the extra
// LocalCost/LocalCurrency columns must parse, and the non-Usage row must be reported, not allocated.
func TestFOCUSParsesTheRealGoogleNativeExportAfterConversion(t *testing.T) {
	f, err := os.Open("testdata/gcp-native-focus-converted.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, e := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	items, sum, err := ParseFOCUS(f, s, e)
	if err != nil || len(items) != 0 || sum.Rows != 1 {
		t.Fatalf("%v %v %+v", err, items, sum)
	}
	if _, ok := sum.Skipped["Adjustment"]; !ok {
		t.Fatalf("the Adjustment row must be listed in Skipped: %+v", sum)
	}
	// The same export WITHOUT the conversion (BillingCurrency CLP) must be refused, never read as dollars.
	raw := "ResourceId,BilledCost,BillingCurrency,ChargeCategory,ChargePeriodStart,ChargePeriodEnd\r\n" +
		"gke-x-node,923.32,CLP,Usage,2026-09-01T07:00:00Z,2026-09-01T08:00:00Z\r\n"
	if _, _, err := ParseFOCUS(strings.NewReader(raw), s, e); err == nil || !strings.Contains(err.Error(), "not USD") {
		t.Fatalf("a CLP export must be refused: %v", err)
	}
}

// testdata/gcp-native-focus-raw.csv is Google's native FOCUS export AS DELIVERED by `bq --format=csv` (2026-11-11; project numbers
// replaced): BigQuery timestamps without "T"/"Z", BillingCurrency CLP with Google's x_CurrencyConversionRate, an empty ListCost.
func TestFOCUSReadsTheRawGoogleExportWithItsOwnConversionRate(t *testing.T) {
	f, err := os.Open("testdata/gcp-native-focus-raw.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, e := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	items, sum, err := ParseFOCUS(f, s, e)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows != 7 || len(items) != 5 || sum.Converted["CLP"] != 7 {
		t.Fatalf("%d items, %+v", len(items), sum)
	}
	if _, ok := sum.Skipped["Adjustment"]; !ok {
		t.Fatalf("adjustments must be reported: %+v", sum)
	}
	if !items[0].Start.Equal(time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("BigQuery timestamp read as %v", items[0].Start)
	}
	notes := strings.Join(sum.Notes, " ")
	if !strings.Contains(notes, "x_CurrencyConversionRate") || !strings.Contains(notes, "x_Credits") {
		t.Fatalf("the conversion and Google's credit layout must be stated: %v", sum.Notes)
	}
}

func TestFOCUSConvertsWithTheProviderRateExactlyAndRefusesBadRates(t *testing.T) {
	hdr := "ResourceId,ResourceName,BilledCost,EffectiveCost,BillingCurrency,x_CurrencyConversionRate,ChargeCategory,ChargePeriodStart,ChargePeriodEnd\n"
	in := hdr + "//compute.googleapis.com/projects/1/zones/z/instances/4711,gke-pool-a,9600,4800,CLP,960,Usage,2026-11-01 00:00:00,2026-11-02 00:00:00 UTC\n"
	items, sum, err := ParseFOCUS(strings.NewReader(in), t0, t1)
	if err != nil || len(items) != 1 || items[0].Cost != usd("10") || items[0].Name != "gke-pool-a" || sum.Converted["CLP"] != 1 {
		t.Fatalf("9600 CLP at 960 CLP/USD is 10 USD: %v %+v %+v", err, items, sum)
	}
	eff, sum, err := ParseFOCUSWith(strings.NewReader(in), t0, t1, FocusOptions{CostColumn: "EffectiveCost"})
	if err != nil || eff[0].Cost != usd("5") || !strings.Contains(strings.Join(sum.Notes, " "), "EffectiveCost") {
		t.Fatalf("EffectiveCost: %v %+v %v", err, eff, sum.Notes)
	}
	for name, bad := range map[string]string{
		"zero rate":  hdr + "x,,1,1,CLP,0,Usage,2026-11-01 00:00:00,2026-11-02 00:00:00\n",
		"empty rate": hdr + "x,,1,1,CLP,,Usage,2026-11-01 00:00:00,2026-11-02 00:00:00\n",
		"text rate":  hdr + "x,,1,1,CLP,abc,Usage,2026-11-01 00:00:00,2026-11-02 00:00:00\n",
	} {
		if _, _, err := ParseFOCUS(strings.NewReader(bad), t0, t1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := ParseFOCUSWith(strings.NewReader(in), t0, t1, FocusOptions{CostColumn: "ListCost"}); err == nil {
		t.Fatal("only BilledCost and EffectiveCost may be allocated")
	}
}

func TestFOCUSTimestampShapesOfRealExports(t *testing.T) {
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"2026-11-01T00:00:00Z", "2026-11-01T00:00:00.0000000Z", "2026-11-01 00:00:00", "2026-11-01 00:00:00 UTC",
		"2026-11-01T00:00:00", "2026-11-01", "2026-10-31T21:00:00-03:00"} {
		got, err := parseFocusTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseFocusTime("11/01/2026"); err == nil {
		t.Error("an ambiguous month/day date must be refused, not guessed")
	}
}

// Azure bills an AKS node pool per VM scale set. The item is split across that scale set's nodes by allocatable CPU, the parts sum
// to the item exactly, a bare instance number never matches, and an unrelated scale set stays unallocated.
func TestFOCUSAzureScaleSetCostIsSplitAcrossItsNodes(t *testing.T) {
	ss := "/subscriptions/s/resourceGroups/MC_rg_aks_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-nodepool1-123-vmss"
	nodes := []Node{
		{Name: "aks-nodepool1-123-vmss000000", ProviderID: "azure://" + ss + "/virtualMachines/0", CPUAllocatable: 1.9},
		{Name: "aks-nodepool1-123-vmss000001", ProviderID: "azure://" + ss + "/virtualMachines/1", CPUAllocatable: 3.8},
	}
	items := []BillingItem{
		{Resource: strings.ToLower(ss), Cost: usd("3")},
		{Resource: "/subscriptions/s/resourcegroups/other/providers/microsoft.compute/virtualmachinescalesets/x-vmss", Cost: usd("7")},
		{Resource: "/subscriptions/s/resourcegroups/rg/providers/microsoft.storage/accounts/a/0", Cost: usd("1")},
	}
	nc, extra := SplitByNodeIdentity(items, nodes)
	if nc["aks-nodepool1-123-vmss000000"] != usd("1") || nc["aks-nodepool1-123-vmss000001"] != usd("2") {
		t.Fatalf("split by CPU: %v", nc)
	}
	if len(extra) != 2 {
		t.Fatalf("other scale set and a resource ending in /0 must stay unallocated: %v", extra)
	}
	odd := splitByCPU(usd("1"), []Node{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	if odd["a"]+odd["b"]+odd["c"] != usd("1") {
		t.Fatalf("parts must sum exactly: %v", odd)
	}
}

func TestFOCUSResourceNameFindsAGoogleNodeWhenTheIdIsNumeric(t *testing.T) {
	nodes := []Node{{Name: "gke-soak-pool-abc", ProviderID: "gce://p/us-central1-a/gke-soak-pool-abc"}}
	items := []BillingItem{{Resource: "//compute.googleapis.com/projects/1/zones/us-central1-a/instances/8812345", Name: "gke-soak-pool-abc", Cost: usd("4")}}
	nc, extra := SplitByNodeIdentity(items, nodes)
	if nc["gke-soak-pool-abc"] != usd("4") || len(extra) != 0 {
		t.Fatalf("%v %v", nc, extra)
	}
}
