// Command association-management demonstrates requirements 6.2.7.
// Each run uses one transaction-local schema and rolls back all fixture objects.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	kingbase "kingbase.com/gorm-driver-kingbase"
)

type Company struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:64"`
}

func (Company) TableName() string { return "doc627_companies" }

type Profile struct {
	ID      uint `gorm:"primaryKey"`
	OwnerID *uint
	Bio     string `gorm:"size:64"`
}

func (Profile) TableName() string { return "doc627_profiles" }

type Pet struct {
	ID      uint `gorm:"primaryKey"`
	OwnerID *uint
	Name    string `gorm:"size:64"`
}

func (Pet) TableName() string { return "doc627_pets" }

type Language struct {
	Code string `gorm:"size:16;primaryKey"`
	Name string `gorm:"size:64"`
}

func (Language) TableName() string { return "doc627_languages" }

type Owner struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:64"`
	CompanyID *uint
	Company   Company    `gorm:"foreignKey:CompanyID;references:ID"`
	Profile   Profile    `gorm:"foreignKey:OwnerID;references:ID"`
	Pets      []Pet      `gorm:"foreignKey:OwnerID;references:ID"`
	Languages []Language `gorm:"many2many:doc627_owner_languages"`
}

func (Owner) TableName() string { return "doc627_owners" }

func main() {
	name := flag.String("case", "association-ok", "association-ok, association-error or checks")
	mode := flag.String("mode", "", "expected actual mode: mysql or oracle")
	flag.Parse()
	cases := map[string]func(*gorm.DB) error{
		"association-ok": associationOK, "association-error": associationError,
		"checks": associationChecks,
	}
	run, known := cases[*name]
	if !known || (*mode != "mysql" && *mode != "oracle") {
		fmt.Fprintln(os.Stderr, "ERROR: specify a supported -case and -mode mysql|oracle")
		os.Exit(2)
	}
	fmt.Printf("case=%s expected_mode=%s\n", *name, *mode)
	db, closePool, err := openDatabase(*mode)
	if err == nil {
		err = run(db)
		closePool()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func associationOK(db *gorm.DB) error {
	return withFixture(db, func(tx *gorm.DB) error {
		owner := newOwner("primary")
		// screenshot:association-ok:start
		if err := tx.Create(&owner).Error; err != nil {
			return err
		}
		var loaded Owner
		query := tx.Preload("Company").Preload("Profile").
			Preload("Pets").Preload("Languages")
		if err := query.First(&loaded, owner.ID).Error; err != nil {
			return err
		}
		if err := verifyGraph(tx, owner, loaded); err != nil {
			return err
		}
		added := Pet{Name: "pet-3"}
		if err := tx.Model(&loaded).Association("Pets").Append(&added); err != nil {
			return err
		}
		// screenshot:association-ok:end
		if err := verifyAppendedPet(tx, owner, added); err != nil {
			return err
		}
		fmt.Println("relations: belongs_to=ok has_one=ok has_many=ok many_to_many=ok")
		fmt.Println("preload: company=1 profile=1 pets=2 languages=2 join_rows=2")
		fmt.Println("append_pets_count=3 primary_foreign_keys_match=true")
		return nil
	})
}

func associationError(db *gorm.DB) error {
	return withFixture(db, func(tx *gorm.DB) error {
		owner := newOwner("error")
		if err := tx.Create(&owner).Error; err != nil {
			return err
		}
		before, err := rowCounts(tx)
		if err != nil {
			return err
		}
		// screenshot:association-error:start
		association := tx.Model(&owner).Association("MissingPets")
		err = association.Append(&Pet{Name: "not-created"})
		if !errors.Is(err, gorm.ErrUnsupportedRelation) {
			return fmt.Errorf("expected ErrUnsupportedRelation, got %v", err)
		}
		count := association.Count()
		if count != 0 || !errors.Is(association.Error, gorm.ErrUnsupportedRelation) {
			return errors.New("unexpected invalid-association Count result")
		}
		// screenshot:association-error:end
		after, checkErr := rowCounts(tx)
		if checkErr != nil {
			return checkErr
		}
		if !reflect.DeepEqual(before, after) {
			return errors.New("invalid association changed database row counts")
		}
		loaded, checkErr := loadGraph(tx, owner.ID)
		if checkErr != nil {
			return checkErr
		}
		if checkErr := verifyGraph(tx, owner, loaded); checkErr != nil {
			return checkErr
		}
		fmt.Println("unsupported_relation=true association_count=0 count_is_valid=false")
		fmt.Println("existing_graph_unchanged=true no_extra_entity_created=true")
		return err // Preserve the failed operation (exit 1).
	})
}

func newOwner(name string) Owner {
	return Owner{
		Name: name, Company: Company{Name: name + "-company"},
		Profile:   Profile{Bio: name + "-bio"},
		Pets:      []Pet{{Name: name + "-pet-1"}, {Name: name + "-pet-2"}},
		Languages: []Language{{Code: "zh-CN", Name: "Chinese"}, {Code: "en-US", Name: "English"}},
	}
}

func loadGraph(tx *gorm.DB, id uint) (Owner, error) {
	var out Owner
	err := tx.Preload("Company").Preload("Profile").Preload("Pets").
		Preload("Languages").First(&out, id).Error
	return out, err
}

func verifyGraph(tx *gorm.DB, want, got Owner) error {
	if want.ID == 0 || got.ID != want.ID || got.Name != want.Name ||
		want.Company.ID == 0 || got.Company.ID != want.Company.ID ||
		got.Company.Name != want.Company.Name || !pointsTo(want.CompanyID, want.Company.ID) ||
		!pointsTo(got.CompanyID, want.Company.ID) || want.Profile.ID == 0 ||
		got.Profile.ID != want.Profile.ID || got.Profile.Bio != want.Profile.Bio ||
		!pointsTo(want.Profile.OwnerID, want.ID) || !pointsTo(got.Profile.OwnerID, want.ID) {
		return errors.New("belongs-to/has-one identity or field mapping mismatch")
	}
	if err := verifyPets(got.Pets, want.Pets, want.ID); err != nil {
		return err
	}
	if !sameLanguages(got.Languages, want.Languages) {
		return errors.New("many-to-many preloaded language values mismatch")
	}
	var links []struct {
		OwnerID      uint
		LanguageCode string
	}
	if err := tx.Table("doc627_owner_languages").Where("owner_id=?", want.ID).
		Order("language_code").Find(&links).Error; err != nil {
		return err
	}
	if len(links) != 2 || links[0].OwnerID != want.ID || links[1].OwnerID != want.ID ||
		links[0].LanguageCode != "en-US" || links[1].LanguageCode != "zh-CN" {
		return errors.New("join table contains unexpected owner/language keys")
	}
	return nil
}

func verifyAppendedPet(tx *gorm.DB, owner Owner, added Pet) error {
	if added.ID == 0 || !pointsTo(added.OwnerID, owner.ID) {
		return errors.New("appended pet identity or owner key missing")
	}
	var stored []Pet
	if err := tx.Where("owner_id=?", owner.ID).Find(&stored).Error; err != nil {
		return err
	}
	want := append(append([]Pet{}, owner.Pets...), added)
	if err := verifyPets(stored, want, owner.ID); err != nil {
		return err
	}
	association := tx.Model(&Owner{ID: owner.ID}).Association("Pets")
	count := association.Count()
	if association.Error != nil || count != 3 {
		return fmt.Errorf("appended pet Count=%d error=%v", count, association.Error)
	}
	return nil
}

func verifyPets(got, want []Pet, ownerID uint) error {
	if len(got) != len(want) {
		return errors.New("pet cardinality mismatch")
	}
	expected := make(map[uint]string, len(want))
	for _, pet := range want {
		if pet.ID == 0 || !pointsTo(pet.OwnerID, ownerID) || expected[pet.ID] != "" {
			return errors.New("invalid source pet keys")
		}
		expected[pet.ID] = pet.Name
	}
	for _, pet := range got {
		if !pointsTo(pet.OwnerID, ownerID) || expected[pet.ID] != pet.Name {
			return errors.New("stored/preloaded pet identity or fields mismatch")
		}
		delete(expected, pet.ID)
	}
	if len(expected) != 0 {
		return errors.New("preload omitted a pet")
	}
	return nil
}

func sameLanguages(got, want []Language) bool {
	if len(got) != len(want) {
		return false
	}
	expected := make(map[string]string, len(want))
	for _, language := range want {
		expected[language.Code] = language.Name
	}
	for _, language := range got {
		if expected[language.Code] != language.Name {
			return false
		}
		delete(expected, language.Code)
	}
	return len(expected) == 0
}

func pointsTo(value *uint, want uint) bool { return value != nil && *value == want }

func rowCounts(tx *gorm.DB) ([]int64, error) {
	counts := make([]int64, 0, 6)
	for _, table := range []string{"doc627_owners", "doc627_companies", "doc627_profiles",
		"doc627_pets", "doc627_languages", "doc627_owner_languages"} {
		var count int64
		if err := tx.Table(table).Count(&count).Error; err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, nil
}

func associationChecks(db *gorm.DB) error {
	return withFixture(db, func(tx *gorm.DB) error {
		owner, other := newOwner("target"), newOwner("other")
		if err := tx.Create(&owner).Error; err != nil {
			return err
		}
		if err := tx.Create(&other).Error; err != nil {
			return err
		}
		for _, source := range []Owner{owner, other} {
			loaded, err := loadGraph(tx, source.ID)
			if err != nil {
				return err
			}
			if err := verifyGraph(tx, source, loaded); err != nil {
				return err
			}
		}
		var joined Owner
		if err := tx.Joins("Company").Joins("Profile").First(&joined, owner.ID).Error; err != nil {
			return err
		}
		if joined.Company.ID != owner.Company.ID || joined.Company.Name != owner.Company.Name ||
			joined.Profile.ID != owner.Profile.ID || joined.Profile.Bio != owner.Profile.Bio {
			return errors.New("single-object Joins mapping mismatch")
		}
		fmt.Println("belongs_to_and_has_one_joins=PASS")
		if err := checkRelation(tx, owner.ID, other.ID, "Company", true,
			&Company{Name: "extra-company-a"}, &Company{Name: "extra-company-b"},
			func(v Company) string { return fmt.Sprint(v.ID) }, func(v Company) string { return v.Name },
			"SELECT c.id FROM doc627_companies c JOIN doc627_owners o ON o.company_id=c.id WHERE o.id=?"); err != nil {
			return err
		}
		if err := checkRelation(tx, owner.ID, other.ID, "Profile", true,
			&Profile{Bio: "extra-profile-a"}, &Profile{Bio: "extra-profile-b"},
			func(v Profile) string { return fmt.Sprint(v.ID) }, func(v Profile) string { return v.Bio },
			"SELECT id FROM doc627_profiles WHERE owner_id=?"); err != nil {
			return err
		}
		if err := checkRelation(tx, owner.ID, other.ID, "Pets", false,
			&Pet{Name: "extra-pet-a"}, &Pet{Name: "extra-pet-b"},
			func(v Pet) string { return fmt.Sprint(v.ID) }, func(v Pet) string { return v.Name },
			"SELECT id FROM doc627_pets WHERE owner_id=?"); err != nil {
			return err
		}
		if err := checkRelation(tx, owner.ID, other.ID, "Languages", false,
			&Language{Code: "extra-a", Name: "extra-language-a"},
			&Language{Code: "extra-b", Name: "extra-language-b"},
			func(v Language) string { return v.Code }, func(v Language) string { return v.Name },
			"SELECT language_code FROM doc627_owner_languages WHERE owner_id=?"); err != nil {
			return err
		}
		loaded, err := loadGraph(tx, other.ID)
		if err != nil {
			return err
		}
		if err := verifyGraph(tx, other, loaded); err != nil {
			return err
		}
		fmt.Println("other_owner_complete_graph_unchanged=true")
		return nil
	})
}

// Verify each API result against independent relationship SQL and all entities.
func checkRelation[T any](tx *gorm.DB, ownerID, otherID uint, relation string, single bool,
	first, second *T, key func(T) string, label func(T) string, linkedSQL string) error {
	readKeys := func(id uint) ([]string, error) {
		keys := make([]string, 0)
		err := tx.Raw(linkedSQL, id).Scan(&keys).Error
		sort.Strings(keys)
		return keys, err
	}
	readEntities := func() (map[string]string, error) {
		var items []T
		if err := tx.Find(&items).Error; err != nil {
			return nil, err
		}
		out := make(map[string]string, len(items))
		for _, item := range items {
			id := key(item)
			if id == "" || id == "0" || out[id] != "" {
				return nil, errors.New("zero or duplicate entity key")
			}
			out[id] = label(item)
		}
		return out, nil
	}
	expected, err := readKeys(ownerID)
	if err != nil {
		return err
	}
	otherExpected, err := readKeys(otherID)
	if err != nil {
		return err
	}
	entities, err := readEntities()
	if err != nil {
		return err
	}
	verify := func(want []string) error {
		sort.Strings(want)
		for _, target := range []struct {
			id   uint
			want []string
		}{{ownerID, want}, {otherID, otherExpected}} {
			actual, err := readKeys(target.id)
			if err != nil || !reflect.DeepEqual(actual, target.want) {
				return fmt.Errorf("%s relationship SQL mismatch: got=%v want=%v error=%v", relation, actual, target.want, err)
			}
			var current Owner
			if err := tx.First(&current, target.id).Error; err != nil {
				return err
			}
			association := tx.Model(&current).Association(relation)
			count := association.Count()
			if association.Error != nil || count != int64(len(target.want)) {
				return fmt.Errorf("%s Count=%d error=%v", relation, count, association.Error)
			}
			var found []T
			if err := tx.Model(&current).Association(relation).Find(&found); err != nil {
				return err
			}
			keys := make([]string, 0, len(found))
			for _, item := range found {
				if entities[key(item)] != label(item) {
					return errors.New("association Find returned wrong entity fields")
				}
				keys = append(keys, key(item))
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, target.want) {
				return fmt.Errorf("%s Find mismatch: got=%v want=%v", relation, keys, target.want)
			}
		}
		actualEntities, err := readEntities()
		if err != nil || !reflect.DeepEqual(actualEntities, entities) {
			return fmt.Errorf("%s entity loss or content change: %v", relation, err)
		}
		return nil
	}
	if err := verify(expected); err != nil {
		return err
	}
	for _, operation := range []string{"append", "replace", "delete", "append-again", "clear"} {
		var current Owner
		if err := tx.First(&current, ownerID).Error; err != nil {
			return err
		}
		association := tx.Model(&current).Association(relation)
		switch operation {
		case "append", "append-again":
			err = association.Append(first)
			if single || operation == "append-again" {
				expected = []string{key(*first)}
			} else {
				expected = append(expected, key(*first))
			}
			entities[key(*first)] = label(*first)
		case "replace":
			err = association.Replace(second)
			expected = []string{key(*second)}
			entities[key(*second)] = label(*second)
		case "delete":
			err = association.Delete(second)
			expected = []string{}
		case "clear":
			err = association.Clear()
			expected = []string{}
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", relation, operation, err)
		}
		if err := verify(expected); err != nil {
			return fmt.Errorf("%s after %s: %w", relation, operation, err)
		}
	}
	fmt.Printf("%s: append_replace_delete_clear=PASS entities_preserved=true other_owner_unchanged=true\n", relation)
	return nil
}

func openDatabase(mode string) (*gorm.DB, func(), error) {
	dsn := os.Getenv("KINGBASE_DSN")
	if dsn == "" {
		return nil, nil, errors.New("KINGBASE_DSN is required; configure the target test instance first")
	}
	db, err := gorm.Open(kingbase.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	closePool := func() { _ = pool.Close() }
	actual := string(db.Dialector.(*kingbase.Dialector).DatabaseMode())
	if actual != mode {
		closePool()
		return nil, nil, fmt.Errorf("actual mode=%s, expected %s", actual, mode)
	}
	fmt.Printf("detected_mode=%s\n", actual)
	return db, closePool, nil
}

func withFixture(db *gorm.DB, body func(*gorm.DB) error) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	namespace := fmt.Sprintf("gorm_doc627_%d", time.Now().UnixNano())
	defer func() {
		rollbackErr := tx.Rollback().Error
		fresh, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var count int64
		cleanupErr := db.WithContext(fresh).Raw(
			"SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname=?", namespace,
		).Row().Scan(&count)
		if cleanupErr == nil && count != 0 {
			cleanupErr = fmt.Errorf("fixture schema remains: count=%d", count)
		}
		if rollbackErr == nil && cleanupErr == nil {
			fmt.Println("fixture_cleanup_count=0")
		}
		err = errors.Join(err, rollbackErr, cleanupErr)
	}()
	if err := tx.Exec("CREATE SCHEMA ?", clause.Column{Name: namespace}).Error; err != nil {
		return err
	}
	var path, current string
	if err := tx.Raw("SELECT set_config('search_path', ?, true)", namespace).
		Row().Scan(&path); err != nil {
		return err
	}
	if err := tx.Raw("SELECT CURRENT_SCHEMA()").Row().Scan(&current); err != nil || current != namespace {
		return fmt.Errorf("unexpected fixture schema %q: %v", current, err)
	}
	if err := tx.AutoMigrate(&Company{}, &Owner{}, &Profile{}, &Pet{}, &Language{}); err != nil {
		return err
	}
	return body(tx)
}
