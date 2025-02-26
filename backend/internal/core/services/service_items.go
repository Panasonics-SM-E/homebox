package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/hay-kot/homebox/backend/internal/core/services/reporting"
	"github.com/hay-kot/homebox/backend/internal/data/repo"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrFileNotFound = errors.New("file not found")
)

type ItemService struct {
	repo *repo.AllRepos

	filepath string

	autoIncrementAssetID bool
}

func (svc *ItemService) Create(ctx Context, item repo.ItemCreate) (repo.ItemOut, error) {
	if svc.autoIncrementAssetID {
		highest, err := svc.repo.Items.GetHighestAssetID(ctx, ctx.GID)
		if err != nil {
			return repo.ItemOut{}, err
		}

		item.AssetID = repo.AssetID(highest + 1)
	}

	return svc.repo.Items.Create(ctx, ctx.GID, item)
}

func (svc *ItemService) EnsureAssetID(ctx context.Context, GID uuid.UUID) (int, error) {
	items, err := svc.repo.Items.GetAllZeroAssetID(ctx, GID)
	if err != nil {
		return 0, err
	}

	highest, err := svc.repo.Items.GetHighestAssetID(ctx, GID)
	if err != nil {
		return 0, err
	}

	finished := 0
	for _, item := range items {
		highest++

		err = svc.repo.Items.SetAssetID(ctx, GID, item.ID, repo.AssetID(highest))
		if err != nil {
			return 0, err
		}

		finished++
	}

	return finished, nil
}

func (svc *ItemService) EnsureImportRef(ctx context.Context, GID uuid.UUID) (int, error) {
	ids, err := svc.repo.Items.GetAllZeroImportRef(ctx, GID)
	if err != nil {
		return 0, err
	}

	finished := 0
	for _, itemID := range ids {
		ref := uuid.New().String()[0:8]

		err = svc.repo.Items.Patch(ctx, GID, itemID, repo.ItemPatch{ImportRef: &ref})
		if err != nil {
			return 0, err
		}

		finished++
	}

	return finished, nil
}

func serializeLocation[T ~[]string](location T) string {
	return strings.Join(location, "/")
}

// Ooi J Sen
// Function to validate headers
func validateHeaders(expected, actual []string) bool {
	actualHeaderCount := make(map[string]int)

	// Count occurrences of headers in the actual slice
	for _, header := range actual {
		actualHeaderCount[header]++
	}

	// Check if all actual headers are within the expected headers
	for header, count := range actualHeaderCount {
		if count > 0 && !contains(expected, header) {
			return false
		}
	}

	return true
}

// Function to check if a string slice contains a specific string
func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

// CsvImport imports items from a CSV file. using the standard defined format.
//
// CsvImport applies the following rules/operations
//
//  1. If the item does not exist, it is created.
//  2. If the item has a ImportRef and it exists it is skipped
//  3. Locations and Labels are created if they do not exist.
func (svc *ItemService) CsvImport(ctx context.Context, GID uuid.UUID, data io.Reader) (int, error) {
	sheet := reporting.IOSheet{}

	err := sheet.Read(data)
	if err != nil {
		return 0, err
	}
	
	// Ooi J Sen
	// Access excel sheet headers
	headers := sheet.GetHeaders()

	// Validate column headers
	expectedHeaders := []string{"HB.import_ref", "HB.location", "HB.labels", "HB.asset_id", "HB.archived", "HB.name", "HB.quantity", "HB.description", "HB.insured", "HB.notes", "HB.purchase_price", "HB.purchase_from", "HB.purchase_time", "HB.manufacturer", "HB.model_number", "HB.serial_number", "HB.lifetime_warranty", "HB.warranty_expires", "HB.warranty_details", "HB.sold_to", "HB.sold_price", "HB.sold_time", "HB.sold_notes",}
	if !validateHeaders(expectedHeaders, headers) {
		return 0, fmt.Errorf("CSV columns do not match the expected format")
	}

	// ========================================
	// Labels

	labelMap := make(map[string]uuid.UUID)
	{
		labels, err := svc.repo.Labels.GetAll(ctx, GID)
		if err != nil {
			return 0, err
		}

		for _, label := range labels {
			labelMap[label.Name] = label.ID
		}
	}

	// ========================================
	// Locations

	locationMap := make(map[string]uuid.UUID)
	{
		locations, err := svc.repo.Locations.Tree(ctx, GID, repo.TreeQuery{WithItems: false})
		if err != nil {
			return 0, err
		}

		// Traverse the tree and build a map of location full paths to IDs
		// where the full path is the location name joined by slashes.
		var traverse func(location *repo.TreeItem, path []string)
		traverse = func(location *repo.TreeItem, path []string) {
			path = append(path, location.Name)

			locationMap[serializeLocation(path)] = location.ID

			for _, child := range location.Children {
				traverse(child, path)
			}
		}

		for _, location := range locations {
			traverse(&location, []string{})
		}
	}

	// ========================================
	// Import items

	// Asset ID Pre-Check
	highestAID := repo.AssetID(-1)
	if svc.autoIncrementAssetID {
		highestAID, err = svc.repo.Items.GetHighestAssetID(ctx, GID)
		if err != nil {
			return 0, err
		}
	}

	finished := 0

	var errorMessage string

	for i := range sheet.Rows {
		row := sheet.Rows[i]
		
		var hasNegativeValues bool

		createRequired := true

		// ========================================
		// Preflight check for existing item
		if row.ImportRef != "" {
			exists, err := svc.repo.Items.CheckRef(ctx, GID, row.ImportRef)
			if err != nil {
				return 0, fmt.Errorf("error checking for existing item with ref %q: %w", row.ImportRef, err)
			}

			if exists {
				createRequired = false
			}
		}
		
		// Ooi J Sen
		// Check integer fields for negative values
		if row.Quantity < 0 {
			errorMessage += fmt.Sprintf("Negative quantity at row %d\n", i+1)
			hasNegativeValues = true
		}
		if row.PurchasePrice < 0 {
			errorMessage += fmt.Sprintf("Negative purchase price at row %d\n", i+1)
			hasNegativeValues = true
		}
		if row.SoldPrice < 0 {
			errorMessage += fmt.Sprintf("Negative sold price at row %d\n", i+1)
			hasNegativeValues = true
		}

		if (hasNegativeValues) {
			continue
		}

		// ========================================
		// Pre-Create Labels as necessary
		labelIds := make([]uuid.UUID, len(row.LabelStr))

		for j := range row.LabelStr {
			label := row.LabelStr[j]

			id, ok := labelMap[label]
			if !ok {
				newLabel, err := svc.repo.Labels.Create(ctx, GID, repo.LabelCreate{Name: label})
				if err != nil {
					return 0, err
				}
				id = newLabel.ID
			}

			labelIds[j] = id
			labelMap[label] = id
		}

		// ========================================
		// Pre-Create Locations as necessary
		path := serializeLocation(row.Location)

		locationID, ok := locationMap[path]
		if !ok { // Traverse the path of LocationStr and check each path element to see if it exists already, if not create it.
			paths := []string{}
			for i, pathElement := range row.Location {
				paths = append(paths, pathElement)
				path := serializeLocation(paths)

				locationID, ok = locationMap[path]
				if !ok {
					parentID := uuid.Nil

					// Get the parent ID
					if i > 0 {
						parentPath := serializeLocation(row.Location[:i])
						parentID = locationMap[parentPath]
					}

					newLocation, err := svc.repo.Locations.Create(ctx, GID, repo.LocationCreate{
						ParentID: parentID,
						Name:     pathElement,
					})
					if err != nil {
						return 0, err
					}
					locationID = newLocation.ID
				}

				locationMap[path] = locationID
			}

			locationID, ok = locationMap[path]
			if !ok {
				return 0, errors.New("failed to create location")
			}
		}

		var effAID repo.AssetID
		if svc.autoIncrementAssetID && row.AssetID.Nil() {
			effAID = highestAID + 1
			highestAID++
		} else {
			effAID = row.AssetID
		}

		// ========================================
		// Create Item
		var item repo.ItemOut
		switch {
		case createRequired:
			newItem := repo.ItemCreate{
				ImportRef:   row.ImportRef,
				Name:        row.Name,
				Description: row.Description,
				AssetID:     effAID,
				LocationID:  locationID,
				LabelIDs:    labelIds,
			}

			item, err = svc.repo.Items.Create(ctx, GID, newItem)
			if err != nil {
				return 0, err
			}
		default:
			item, err = svc.repo.Items.GetByRef(ctx, GID, row.ImportRef)
			if err != nil {
				return 0, err
			}
		}

		if item.ID == uuid.Nil {
			panic("item ID is nil on import - this should never happen")
		}

		fields := make([]repo.ItemField, len(row.Fields))
		for i := range row.Fields {
			fields[i] = repo.ItemField{
				Name:      row.Fields[i].Name,
				Type:      "text",
				TextValue: row.Fields[i].Value,
			}
		}

		updateItem := repo.ItemUpdate{
			ID:         item.ID,
			LabelIDs:   labelIds,
			LocationID: locationID,

			Name:        row.Name,
			Description: row.Description,
			AssetID:     effAID,
			Insured:     row.Insured,
			Quantity:    row.Quantity,
			Archived:    row.Archived,

			PurchasePrice: row.PurchasePrice,
			PurchaseFrom:  row.PurchaseFrom,
			PurchaseTime:  row.PurchaseTime,

			Manufacturer: row.Manufacturer,
			ModelNumber:  row.ModelNumber,
			SerialNumber: row.SerialNumber,

			LifetimeWarranty: row.LifetimeWarranty,
			WarrantyExpires:  row.WarrantyExpires,
			WarrantyDetails:  row.WarrantyDetails,

			SoldTo:    row.SoldTo,
			SoldTime:  row.SoldTime,
			SoldPrice: row.SoldPrice,
			SoldNotes: row.SoldNotes,

			Notes:  row.Notes,
			Fields: fields,
		}

		item, err = svc.repo.Items.UpdateByGroup(ctx, GID, updateItem)
		if err != nil {
			return 0, err
		}

		finished++
	}

	// Ooi J Sen
	// Display error messages in console
	if errorMessage != "" {
		// Log or handle the error message here
		fmt.Println("Error Messages:")
		fmt.Println(errorMessage)
		return 0, fmt.Errorf("Errors detected in CSV:\n%s", errorMessage)
	}

	return finished, nil
}

func (svc *ItemService) ExportTSV(ctx context.Context, GID uuid.UUID) ([][]string, error) {
	items, err := svc.repo.Items.GetAll(ctx, GID)
	if err != nil {
		return nil, err
	}

	sheet := reporting.IOSheet{}

	err = sheet.ReadItems(ctx, items, GID, svc.repo)
	if err != nil {
		return nil, err
	}

	return sheet.TSV()
}

func (svc *ItemService) ExportBillOfMaterialsTSV(ctx context.Context, GID uuid.UUID) ([]byte, error) {
	items, err := svc.repo.Items.GetAll(ctx, GID)
	if err != nil {
		return nil, err
	}

	return reporting.BillOfMaterialsTSV(items)
}
