package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type importResult struct {
	Committed  bool `json:"committed"`
	Rows       int  `json:"rows"`
	Categories int  `json:"categories"`
	Items      int  `json:"items"`
	Variants   int  `json:"variants"`
	Errors     []struct {
		Row     int    `json:"row"`
		Column  string `json:"column"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (e *env) importCSV(t *testing.T, tok, file string, dryRun bool) importResult {
	t.Helper()
	r := e.do(t, "POST", fmt.Sprintf("/v1/catalog/import?dry_run=%t", dryRun), tok, file)
	if r.Code != http.StatusOK {
		t.Fatalf("import: %d %s", r.Code, r.Body.String())
	}
	var res importResult
	r.decode(t, &res)
	return res
}

// A trimmed Moka item export: a byte order mark, columns Orion ignores, one item per variant row,
// prices typed several ways and a blank row.
const mokaExport = "\xef\xbb\xbfInternal ID Variant,Category,SKU,Items Name,Small Image for POS,Variant Name,Basic - Price,Track Stock\n" +
	",Kopi,KS-R,Kopi Susu,,Regular,\"18,000\",Yes\n" +
	",Kopi,KS-L,Kopi Susu,,Large,22000,No\n" +
	",,,,,,,\n" +
	",Makanan,,Roti Bakar,,,Rp 15.000,\n" +
	",,,Air Mineral,,,5000.00,\n"

func TestImportCatalogFromMokaExport(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	want := importResult{Rows: 4, Categories: 2, Items: 3, Variants: 4}

	// The dry run reports what would be created and writes nothing.
	dry := e.importCSV(t, tok, mokaExport, true)
	if dry.Committed || dry.Rows != want.Rows || dry.Categories != 2 || dry.Items != 3 || dry.Variants != 4 || len(dry.Errors) != 0 {
		t.Fatalf("dry run = %+v", dry)
	}
	if n := e.count(t, `SELECT count(*) FROM item`); n != 0 {
		t.Fatalf("the dry run wrote %d items", n)
	}

	res := e.importCSV(t, tok, mokaExport, false)
	if !res.Committed || res.Categories != 2 || res.Items != 3 || res.Variants != 4 || len(res.Errors) != 0 {
		t.Fatalf("import = %+v", res)
	}
	var cats struct{ Items []categoryBody }
	e.do(t, "GET", "/v1/categories", tok, nil).decode(t, &cats)
	catName := map[string]string{}
	for _, c := range cats.Items {
		catName[c.ID] = c.Name
	}
	var list struct{ Items []itemBody }
	e.do(t, "GET", "/v1/items", tok, nil).decode(t, &list)
	items := map[string]itemBody{}
	for _, it := range list.Items {
		items[it.Name] = it
	}
	kopi, roti, air := items["Kopi Susu"], items["Roti Bakar"], items["Air Mineral"]
	if len(items) != 3 || len(kopi.Variants) != 2 || !kopi.TrackStock || kopi.CategoryID == nil || catName[*kopi.CategoryID] != "Kopi" {
		t.Fatalf("items = %+v, categories %v", items, catName)
	}
	if v := kopi.Variants; v[0].Name != "Regular" || v[0].BasePrice != 18000 || *v[0].SKU != "KS-R" || v[1].Name != "Large" || v[1].BasePrice != 22000 {
		t.Errorf("Kopi Susu variants = %+v", v)
	}
	if len(roti.Variants) != 1 || roti.Variants[0].BasePrice != 15000 || roti.Variants[0].Name != "" || roti.TrackStock || catName[*roti.CategoryID] != "Makanan" {
		t.Errorf("Roti Bakar = %+v", roti)
	}
	if air.CategoryID != nil || air.Variants[0].BasePrice != 5000 {
		t.Errorf("Air Mineral = %+v", air)
	}
	// Tablets get the new catalog in their next pull, and the import is in the audit log.
	if n := e.count(t, `SELECT count(*) FROM change_log WHERE entity_type IN ('category', 'item')`); n != 5 {
		t.Errorf("%d change log entries, want 5", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tenant_audit_log WHERE action = 'catalog.imported'`); n != 1 {
		t.Errorf("%d audit entries, want 1", n)
	}

	// Sending the same file again adds nothing: every item is already there.
	again := e.importCSV(t, tok, mokaExport, false)
	if again.Committed || len(again.Errors) != 4 || again.Errors[0].Row != 2 || again.Errors[0].Column != "item_name" {
		t.Errorf("second import = %+v", again)
	}
	if n := e.count(t, `SELECT count(*) FROM item`); n != 3 {
		t.Errorf("%d items after the second import, want 3", n)
	}
}

// Every row error is reported with its line and column, and one bad row stops the whole file.
func TestImportCatalogReportsRowErrorsAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	var cat categoryBody
	e.create(t, "/v1/categories", tok, map[string]any{"name": "Kopi"}, &cat)
	var it itemBody
	e.create(t, "/v1/items", tok, map[string]any{"name": "Latte", "variants": []map[string]any{{"base_price": 1, "sku": "LAT"}}}, &it)

	// Semicolons, as Excel writes CSV in an Indonesian locale, after a byte order mark on a column Orion reads.
	file := "\xef\xbb\xbfkategori;nama item;nama varian;harga;sku;track stock\n" +
		"KOPI;Americano;;20000;;\n" + // 2: fine, in the existing category
		"Kopi;Mocha;;18.5;;\n" + // 3: cents
		"Kopi;Teh;;10000;;maybe\n" + // 4: bad track stock
		"Kopi;Latte;;25000;;\n" + // 5: already in the catalog
		"Kopi;Es Teh;;8000;ET;\n" + // 6: fine
		"Kopi;Es Teh;;9000;;\n" + // 7: same item, no variant name
		"Teh;Es Teh;Besar;9000;;\n" + // 8: same item, other category
		"Kopi;Jus;;12000;ET;\n" + // 9: SKU also on line 6
		"Kopi;Soda;;12000;LAT;\n" // 10: SKU already in the catalog
	res := e.importCSV(t, tok, file, false)
	type rc struct {
		row    int
		column string
	}
	var got []rc
	for _, er := range res.Errors {
		got = append(got, rc{er.Row, er.Column})
	}
	want := []rc{{3, "price"}, {4, "track_stock"}, {5, "item_name"}, {7, "variant_name"}, {8, "category"}, {9, "sku"}, {10, "sku"}}
	if fmt.Sprint(got) != fmt.Sprint(want) || res.Committed || res.Rows != 9 {
		t.Errorf("errors = %v (%+v), want %v", got, res, want)
	}
	if res.Categories != 0 || res.Items != 2 || res.Variants != 2 {
		t.Errorf("counts = %+v, want the existing category reused, and Americano and Es Teh", res)
	}
	if n := e.count(t, `SELECT count(*) FROM item`); n != 1 {
		t.Errorf("%d items, want only Latte", n)
	}
}

func TestImportCatalogRefusesBadFiles(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	tooMany := "item_name,price\n" + strings.Repeat("Kopi,1\n", 2001)
	for name, file := range map[string]string{
		"empty":      "",
		"no price":   "item_name,category\nKopi,Minuman\n",
		"not UTF-8":  "item_name,price\nKop\xe9,1\n",
		"too many":   tooMany,
		"bad quotes": "item_name,price\n\"Kopi,1\n",
		"two names":  "item name,items name,price\n",
	} {
		t.Run(name, func(t *testing.T) {
			e.do(t, "POST", "/v1/catalog/import", tok, file).problem(t, http.StatusBadRequest, "validation_failed")
		})
	}
	big := "item_name,price\n" + strings.Repeat("Kopi Susu Gula Aren Dengan Topping,25000\n", 30000)
	e.do(t, "POST", "/v1/catalog/import", tok, big).problem(t, http.StatusRequestEntityTooLarge, "payload_too_large")
}

// The largest file commits, with the same number of queries as a two-row file.
func TestImportCatalogQueryCountIsFixed(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	file := func(prefix string, rows int) string {
		var b strings.Builder
		b.WriteString("category,item_name,variant_name,price\n")
		for i := range rows {
			fmt.Fprintf(&b, "Cat %d,%s %d,%s,1000\n", i/2%20, prefix, i/2, []string{"S", "L"}[i%2])
		}
		return b.String()
	}
	var small, big importResult
	few := e.d.Queries.During(func() { small = e.importCSV(t, tok, file("A", 2), false) })
	start := time.Now()
	many := e.d.Queries.During(func() { big = e.importCSV(t, tok, file("B", 2000), false) })
	t.Logf("2000 rows in %v", time.Since(start))
	if !small.Committed || !big.Committed || big.Items != 1000 || big.Variants != 2000 || big.Categories != 19 {
		t.Fatalf("small %+v, big %+v", small, big)
	}
	if few != many {
		t.Errorf("%d queries for 2 rows, %d for 2000", few, many)
	}
}
