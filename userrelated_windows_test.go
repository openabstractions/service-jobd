package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
)

const (
	codeA = "{2692D2EB-EC1F-443C-BC91-4C654895184C}"
	codeB = "{A009F737-A7FA-48D8-8C0A-CEB4DAB49166}"
)

func fakeProductInfo(values map[string]map[string]string, failures map[string]error) productInfoFunc {
	return func(code, property string) (string, error) {
		if err, ok := failures[code+"/"+property]; ok {
			return "", err
		}
		product, ok := values[code]
		if !ok {
			return "", errorUnknownProduct
		}
		value, ok := product[property]
		if !ok {
			return "", errorUnknownProperty
		}
		return value, nil
	}
}

func TestRelatedProductCodes(t *testing.T) {
	codes, err := relatedProductCodes(strings.ToLower(codeA) + ";" + codeB + ";" + codeA)
	if err != nil || len(codes) != 2 || codes[0] != codeA || codes[1] != codeB {
		t.Fatalf("codes=%v err=%v", codes, err)
	}
	for _, bad := range []string{"", ";", codeA + ";", "[WIX_UPGRADE_DETECTED]", strings.Trim(codeA, "{}"), codeA + " ;" + codeB, `"` + codeA + `"`} {
		if _, err := relatedProductCodes(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestRelatedProductsReadRegistration(t *testing.T) {
	values := map[string]map[string]string{
		codeA: {"VersionString": "0.1.5", "InstallLocation": `D:\Tools\OpenAbstractions\`},
		codeB: {"VersionString": "0.1.6"},
	}
	products, err := relatedProducts(codeA+";"+codeB, fakeProductInfo(values, nil))
	if err != nil || len(products) != 2 {
		t.Fatalf("products=%v err=%v", products, err)
	}
	if products[0].location != `D:\Tools\OpenAbstractions\` || products[0].version != "0.1.5" || products[1].location != "" {
		t.Fatalf("products=%+v", products)
	}
	if _, err := relatedProducts("{00000000-0000-0000-0000-000000000001}", fakeProductInfo(values, nil)); err == nil || !strings.Contains(err.Error(), "not installed for this account") {
		t.Fatalf("unregistered related product accepted: %v", err)
	}
	denied := map[string]error{codeA + "/InstallLocation": syscall.Errno(5)}
	if _, err := relatedProducts(codeA, fakeProductInfo(values, denied)); err == nil || !errors.Is(err, syscall.Errno(5)) {
		t.Fatalf("registration read failure hidden: %v", err)
	}
}

func TestUpgradeFoldersIncludeRecordedLocations(t *testing.T) {
	related := []relatedProduct{
		{code: codeA, version: "0.1.5", location: `D:\Tools\OpenAbstractions\`},
		{code: codeB, version: "0.1.6", location: testFolder + `\`},
		{code: "{00000000-0000-0000-0000-000000000002}", version: "0.1.4"},
	}
	folders, notes, err := upgradeFolders(testFolder+`\.`, related)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || !strings.EqualFold(folders[0], testFolder) || !strings.EqualFold(folders[1], `D:\Tools\OpenAbstractions`) {
		t.Fatalf("folders=%v", folders)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "records no install location") {
		t.Fatalf("notes=%v", notes)
	}
	if _, _, err := upgradeFolders(testFolder, []relatedProduct{{code: codeA, location: `relative\folder`}}); err == nil {
		t.Fatal("relative recorded location accepted")
	}
	if _, _, err := upgradeFolders(testFolder, []relatedProduct{{code: codeA, location: `D:\`}}); err == nil {
		t.Fatal("volume root recorded location accepted")
	}
}

func TestUserStopCoversEveryUpgradeFolder(t *testing.T) {
	moved := `D:\Tools\OpenAbstractions`
	inMoved := held(moved+`\tools\jobdw.exe`, testSID, 900)
	inDefault := held(testFolder+`\tools\openabstractions.exe`, testSID, 901)
	elsewhere := held(`D:\Tools\Other\jobd.exe`, testSID, 902)
	ops, _ := fakeOps([][]processEntry{{{10, "jobdw.exe"}, {11, "openabstractions.exe"}, {12, "jobd.exe"}}},
		map[uint32]*fakeHeld{10: inMoved, 11: inDefault, 12: elsewhere})
	stopped, err := stopUserPredecessors(context.Background(), []string{testFolder, moved}, testSID, ops)
	if err != nil || stopped != 2 || inMoved.terminated != 1 || inDefault.terminated != 1 || elsewhere.terminated != 0 {
		t.Fatalf("stopped=%d err=%v moved=%d default=%d elsewhere=%d", stopped, err, inMoved.terminated, inDefault.terminated, elsewhere.terminated)
	}
	// The default folder alone misses a predecessor installed elsewhere.
	missed := held(moved+`\tools\jobdw.exe`, testSID, 900)
	ops, _ = fakeOps([][]processEntry{{{10, "jobdw.exe"}}}, map[uint32]*fakeHeld{10: missed})
	if stopped, err := stopUserPredecessors(context.Background(), []string{testFolder}, testSID, ops); err != nil || stopped != 0 || missed.terminated != 0 {
		t.Fatalf("stopped=%d err=%v", stopped, err)
	}
}

func TestUserStopRelatedArguments(t *testing.T) {
	request, err := serviceArguments([]string{"stop", "--user", testFolder + `\.`, "--related", codeA + ";" + codeB})
	if err != nil || request.command != "stop" || request.userFolder != testFolder+`\.` || request.related != codeA+";"+codeB {
		t.Fatal(request, err)
	}
	for _, args := range [][]string{{"stop", "--user", testFolder, "--related"}, {"stop", "--user", testFolder, "--related", ""},
		{"stop", "--user", testFolder, "--other", codeA}, {"stop", "--related", codeA, "--user", testFolder},
		{"uninstall", "--user", testFolder, "--related", codeA}} {
		if _, err := serviceArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// The binding reaches msi.dll and reports an unregistered product without
// changing anything.
func TestMsiProductInfoReportsUnknownProduct(t *testing.T) {
	_, err := msiProductInfo("{00000000-0000-0000-0000-00000000F00D}", "VersionString")
	if !errors.Is(err, errorUnknownProduct) {
		t.Fatalf("err=%v, want ERROR_UNKNOWN_PRODUCT", err)
	}
}
