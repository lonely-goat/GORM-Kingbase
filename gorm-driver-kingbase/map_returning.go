package kingbase

import (
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
)

// GORM v1.31.1 Scan accepts *[]map, but attempts to scan each RETURNING
// scalar directly into a map when Dest is []map. Adapt only the create step,
// then restore Dest before associations/hooks. Maps retain their original
// identity; CreateInBatches also reaches this path with value subslices.
func createWithMapReturning(config *callbacks.Config) func(*gorm.DB) {
	create := callbacks.Create(config)
	return func(db *gorm.DB) {
		var input []map[string]interface{}
		switch dest := db.Statement.Dest.(type) {
		case []map[string]interface{}:
			input = dest
		case *[]map[string]interface{}:
			if dest == nil {
				create(db)
				return
			}
			input = *dest
		default:
			create(db)
			return
		}
		// Scan appends result maps. A batch subslice can share spare capacity
		// with the next batch; never let those appends overwrite future input.
		batch := append([]map[string]interface{}(nil), input...)
		original := db.Statement.Dest
		db.Statement.Dest = &batch
		defer func() {
			db.Statement.Dest = original
			if dest, ok := original.(*[]map[string]interface{}); ok {
				*dest = batch
			}
		}()
		create(db)
		mergeMapReturning(db)
	}
}

// mergeMapReturning repairs GORM v1.31.1's *[]map scan shape for ordinary
// INSERT ... RETURNING: Scan appends result-only maps after the input maps.
// Row order is usable only when every inserted input returned one result, so
// conflict clauses are deliberately excluded.
func mergeMapReturning(db *gorm.DB) {
	if db.Error != nil || db.DryRun || db.RowsAffected <= 0 {
		return
	}
	if _, hasConflict := db.Statement.Clauses["ON CONFLICT"]; hasConflict {
		return
	}
	batch, ok := db.Statement.Dest.(*[]map[string]interface{})
	if !ok || batch == nil {
		return
	}
	count := int(db.RowsAffected)
	if count > len(*batch)/2 || len(*batch) != 2*count {
		return
	}
	for i := 0; i < count; i++ {
		if (*batch)[i] == nil {
			(*batch)[i] = make(map[string]interface{})
		}
		for key, value := range (*batch)[count+i] {
			(*batch)[i][key] = value
		}
	}
	*batch = (*batch)[:count]
}
