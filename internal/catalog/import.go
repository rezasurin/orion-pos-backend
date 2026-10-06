package catalog

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// MaxImportRows bounds a CSV import. A cafe menu is a few hundred rows; the file is written in one
// transaction under the tenant lock, which this keeps short.
// ponytail: synchronous, not the river job the plan named; move it to a job if files outgrow this.
const MaxImportRows = 2000

// ImportResult is what an import created, or would create on a dry run, and the rows at fault.
type ImportResult struct {
	Committed  bool
	Rows       int
	Categories int
	Items      int
	Variants   int
	Errors     []ImportError
}

// ImportError is a problem with one row. Row is the line in the file; the header is line 1.
type ImportError struct {
	Row     int
	Column  string
	Message string
}

// importHeaders maps a header, lowercased with underscores and repeated spaces folded to one
// space, to the field it fills. Moka's item export headers are included, so its file imports as is;
// its other columns are ignored.
var importHeaders = map[string]string{
	"category": "category", "kategori": "category",
	"item name": "item_name", "items name": "item_name", "nama item": "item_name", "nama produk": "item_name",
	"variant name": "variant_name", "nama varian": "variant_name",
	"price": "price", "basic - price": "price", "harga": "price",
	"sku":         "sku",
	"barcode":     "barcode",
	"track stock": "track_stock",
}

type importRow struct {
	line                                  int
	category, item, variant, sku, barcode string
	price                                 kernel.Rupiah
	trackStock                            bool
}

// ImportCSV adds the categories, items and variants in a CSV file. It only adds: an item already
// in the catalog is a row error. With dryRun, or when any row has an error, nothing is written.
// Problems with the file as a whole (not CSV, missing columns, too many rows) are validation
// errors; problems with rows are in the result.
func (s *Service) ImportCSV(ctx context.Context, tenantID uuid.UUID, r io.Reader, dryRun bool) (ImportResult, error) {
	data, err := io.ReadAll(r) // the HTTP server bounds the body
	if err != nil {
		return ImportResult{}, err
	}
	rows, rowErrs, err := parseImport(data)
	if err != nil {
		return ImportResult{}, err
	}
	var res ImportResult
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if !dryRun {
			if err := kernel.LockTenant(ctx, tx); err != nil {
				return err
			}
		}
		q := db.New(tx)
		ex, err := loadExisting(ctx, q, tenantID)
		if err != nil {
			return err
		}
		res = ImportResult{Rows: len(rows) + countRows(rowErrs), Errors: slices.Clone(rowErrs)}
		p := planImport(rows, ex, &res)
		if dryRun || len(res.Errors) > 0 {
			return nil
		}
		if err := p.write(ctx, tx, q, tenantID); err != nil {
			return mapErr(err)
		}
		res.Committed = true
		return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "catalog.imported", TargetType: "catalog",
			Detail: map[string]any{"categories": res.Categories, "items": res.Items, "variants": res.Variants},
		})
	})
	slices.SortStableFunc(res.Errors, func(a, b ImportError) int { return a.Row - b.Row })
	return res, err
}

// countRows counts the distinct rows that failed to parse, which planImport never sees.
func countRows(errs []ImportError) int {
	n, last := 0, 0
	for _, e := range errs {
		if e.Row != last {
			n, last = n+1, e.Row
		}
	}
	return n
}

func parseImport(data []byte) ([]importRow, []ImportError, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // Excel's "CSV UTF-8" starts with a byte order mark
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("%w: the file is not UTF-8; in Excel save it as \"CSV UTF-8\"", kernel.ErrValidation)
	}
	cr := csv.NewReader(bytes.NewReader(data))
	// Excel in an Indonesian locale separates with semicolons.
	if first, _, _ := bytes.Cut(data, []byte("\n")); bytes.Count(first, []byte(";")) > bytes.Count(first, []byte(",")) {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: the file is empty", kernel.ErrValidation)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the file is not valid CSV: %v", kernel.ErrValidation, err)
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(h, "_", " "))), " ")
		f, ok := importHeaders[h]
		if !ok {
			continue
		}
		if _, dup := col[f]; dup {
			return nil, nil, fmt.Errorf("%w: two columns are both read as %s", kernel.ErrValidation, f)
		}
		col[f] = i
	}
	for _, f := range []string{"item_name", "price"} {
		if _, ok := col[f]; !ok {
			return nil, nil, fmt.Errorf("%w: the file needs an %s column (see the import template)", kernel.ErrValidation, f)
		}
	}

	var rows []importRow
	var errs []ImportError
	for n := 0; ; {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the file is not valid CSV: %v", kernel.ErrValidation, err)
		}
		get := func(f string) string {
			if i, ok := col[f]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue
		}
		if n++; n > MaxImportRows {
			return nil, nil, fmt.Errorf("%w: the file has more than %d rows", kernel.ErrValidation, MaxImportRows)
		}
		line, _ := cr.FieldPos(0)
		row := importRow{
			line: line, category: get("category"), item: get("item_name"), variant: get("variant_name"),
			sku: get("sku"), barcode: get("barcode"),
		}
		bad := func(column, format string, a ...any) {
			errs = append(errs, ImportError{Row: line, Column: column, Message: fmt.Sprintf(format, a...)})
		}
		if row.item == "" || utf8.RuneCountInString(row.item) > maxItemName {
			bad("item_name", "the item name is required and at most %d characters", maxItemName)
		}
		for _, c := range []struct {
			field, v string
			max      int
		}{{"category", row.category, maxName}, {"variant_name", row.variant, maxName}, {"sku", row.sku, maxCode}, {"barcode", row.barcode, maxCode}} {
			if utf8.RuneCountInString(c.v) > c.max {
				bad(c.field, "at most %d characters", c.max)
			}
		}
		if p, ok := parseRupiah(get("price")); ok {
			row.price = p
		} else {
			bad("price", "%q is not a price in whole rupiah", get("price"))
		}
		switch strings.ToLower(get("track_stock")) {
		case "", "no", "n", "tidak", "false", "0":
		case "yes", "y", "ya", "true", "1":
			row.trackStock = true
		default:
			bad("track_stock", "use yes or no")
		}
		if len(errs) == 0 || errs[len(errs)-1].Row != line {
			rows = append(rows, row)
		}
	}
	return rows, errs, nil
}

var (
	plainRupiah   = regexp.MustCompile(`^\d+$`)
	groupedRupiah = regexp.MustCompile(`^\d{1,3}([.,]\d{3})+$`) // 25.000 or 25,000
	zeroSen       = regexp.MustCompile(`^(\d+)[.,]0{1,2}$`)     // 25000.00 from a spreadsheet
)

// parseRupiah reads a whole-rupiah price as people type it: 25000, 25.000, 25,000, Rp 25.000 or
// 25000.00. Anything with real cents, or ambiguous, is refused rather than guessed.
func parseRupiah(s string) (kernel.Rupiah, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "Rp"), "rp")), " ", "")
	switch {
	case plainRupiah.MatchString(s):
	case groupedRupiah.MatchString(s):
		s = strings.NewReplacer(".", "", ",", "").Replace(s)
	case zeroSen.MatchString(s):
		s = zeroSen.FindStringSubmatch(s)[1]
	default:
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v > maxMoney {
		return 0, false
	}
	return kernel.Rupiah(v), true
}

// existing is what the catalog already holds that an import must not clash with.
type existing struct {
	items      map[string]bool      // live item names, lowercased
	categories map[string]uuid.UUID // live category names, lowercased
	codes      map[string]bool      // "sku:X" and "barcode:X" of live variants
}

func loadExisting(ctx context.Context, q *db.Queries, tenantID uuid.UUID) (existing, error) {
	ex := existing{items: map[string]bool{}, categories: map[string]uuid.UUID{}, codes: map[string]bool{}}
	names, err := q.ListLiveItemNames(ctx, tenantID)
	if err != nil {
		return ex, err
	}
	for _, n := range names {
		ex.items[n] = true
	}
	cats, err := q.ListAllCategories(ctx, tenantID)
	if err != nil {
		return ex, err
	}
	for _, c := range cats {
		if c.ArchivedAt == nil {
			ex.categories[strings.ToLower(c.Name)] = c.ID
		}
	}
	codes, err := q.ListLiveVariantCodes(ctx, tenantID)
	if err != nil {
		return ex, err
	}
	for _, c := range codes {
		if c.Sku != nil {
			ex.codes["sku:"+*c.Sku] = true
		}
		if c.Barcode != nil {
			ex.codes["barcode:"+*c.Barcode] = true
		}
	}
	return ex, nil
}

type importItem struct {
	id         uuid.UUID
	name       string
	category   string // lowercased; "" for none
	trackStock bool
	variants   []importRow
}

type importPlan struct {
	newCategories map[string]uuid.UUID // lowercased name to new id
	catNames      map[string]string    // lowercased name to the spelling first seen
	catIDs        map[string]uuid.UUID // every category an item uses, new or existing
	items         []*importItem
}

// planImport groups rows into items and checks them against each other and the catalog, adding row
// errors and the counts to res.
func planImport(rows []importRow, ex existing, res *ImportResult) importPlan {
	p := importPlan{newCategories: map[string]uuid.UUID{}, catNames: map[string]string{}, catIDs: map[string]uuid.UUID{}}
	byName := map[string]*importItem{}
	codeLine := map[string]int{}
	for _, r := range rows {
		bad := func(column, format string, a ...any) {
			res.Errors = append(res.Errors, ImportError{Row: r.line, Column: column, Message: fmt.Sprintf(format, a...)})
		}
		key, cat := strings.ToLower(r.item), strings.ToLower(r.category)
		if ex.items[key] {
			bad("item_name", "an item named %q is already in the catalog", r.item)
			continue
		}
		it := byName[key]
		if it == nil {
			it = &importItem{id: kernel.NewID(), name: r.item, category: cat}
			byName[key] = it
			p.items = append(p.items, it)
		} else if it.category != cat {
			bad("category", "item %q is in a different category on an earlier row", r.item)
			continue
		}
		dupVariant := slices.ContainsFunc(it.variants, func(v importRow) bool { return strings.EqualFold(v.variant, r.variant) })
		switch {
		case dupVariant && r.variant == "":
			bad("variant_name", "item %q is listed more than once; give each row a variant name", r.item)
			continue
		case dupVariant:
			bad("variant_name", "item %q already has a variant %q", r.item, r.variant)
			continue
		case len(it.variants) == maxVariants:
			bad("variant_name", "an item has at most %d variants", maxVariants)
			continue
		}
		clash := false
		for _, c := range []struct{ field, v string }{{"sku", r.sku}, {"barcode", r.barcode}} {
			if c.v == "" {
				continue
			}
			k := c.field + ":" + c.v
			switch {
			case ex.codes[k]:
				bad(c.field, "%s %q is already used in the catalog", c.field, c.v)
				clash = true
			case codeLine[k] != 0:
				bad(c.field, "%s %q is also on line %d", c.field, c.v, codeLine[k])
				clash = true
			default:
				codeLine[k] = r.line
			}
		}
		if clash {
			continue
		}
		it.trackStock = it.trackStock || r.trackStock
		it.variants = append(it.variants, r)
		if cat != "" && p.catIDs[cat] == uuid.Nil {
			if id, ok := ex.categories[cat]; ok {
				p.catIDs[cat] = id
			} else {
				id = kernel.NewID()
				p.catIDs[cat], p.newCategories[cat], p.catNames[cat] = id, id, r.category
			}
		}
	}
	// An item every row of which was refused is not created.
	p.items = slices.DeleteFunc(p.items, func(it *importItem) bool { return len(it.variants) == 0 })
	res.Categories, res.Items = len(p.newCategories), len(p.items)
	for _, it := range p.items {
		res.Variants += len(it.variants)
	}
	return p
}

// write inserts the plan with one statement per table and records the changes for the POS pull.
func (p importPlan) write(ctx context.Context, tx pgx.Tx, q *db.Queries, tenantID uuid.UUID) error {
	cats := db.ImportCategoriesParams{TenantID: tenantID}
	for k, id := range p.newCategories {
		cats.Ids, cats.Names = append(cats.Ids, id), append(cats.Names, p.catNames[k])
	}
	items := db.ImportItemsParams{TenantID: tenantID}
	vs := db.ImportVariantsParams{TenantID: tenantID}
	for _, it := range p.items {
		catID := ""
		if it.category != "" {
			catID = p.catIDs[it.category].String()
		}
		items.Ids, items.CategoryIds = append(items.Ids, it.id), append(items.CategoryIds, catID)
		items.Names, items.TrackStocks = append(items.Names, it.name), append(items.TrackStocks, it.trackStock)
		for i, v := range it.variants {
			vs.Ids, vs.ItemIds, vs.Names = append(vs.Ids, kernel.NewID()), append(vs.ItemIds, it.id), append(vs.Names, v.variant)
			vs.Skus, vs.Barcodes = append(vs.Skus, v.sku), append(vs.Barcodes, v.barcode)
			vs.BasePrices, vs.SortOrders = append(vs.BasePrices, int64(v.price)), append(vs.SortOrders, int32(i)) //nolint:gosec // at most maxVariants
		}
	}
	if err := q.ImportCategories(ctx, cats); err != nil {
		return err
	}
	if err := q.ImportItems(ctx, items); err != nil {
		return err
	}
	if err := q.ImportVariants(ctx, vs); err != nil {
		return err
	}
	if err := kernel.RecordChanges(ctx, tx, EntityCategory, cats.Ids); err != nil {
		return err
	}
	return kernel.RecordChanges(ctx, tx, EntityItem, items.Ids)
}
