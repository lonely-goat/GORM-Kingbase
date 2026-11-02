package integration_test

import (
	"testing"

	"gorm.io/gorm"
)

type indexCommentSimpleProbe struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:111;index:idx_kb_comment_simple,comment:这是一个index"`
}

func (indexCommentSimpleProbe) TableName() string { return "gorm_kb_accept_index_comment_simple" }

type indexCommentQuotedProbe struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:111;index:idx_kb_comment_quoted,comment:owner's index"`
}

func (indexCommentQuotedProbe) TableName() string { return "gorm_kb_accept_index_comment_quoted" }

func TestIndexCommentMigration(t *testing.T) {
	runAcceptanceModes(t, nil, func(t *testing.T, db *gorm.DB, _ acceptanceMode) {
		for _, tc := range []struct {
			name, indexName, want string
			model                 interface{}
		}{
			{name: "simple", indexName: "idx_kb_comment_simple", want: "这是一个index", model: &indexCommentSimpleProbe{}},
			{name: "quote", indexName: "idx_kb_comment_quoted", want: "owner's index", model: &indexCommentQuotedProbe{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAcceptanceTables(t, db, tc.model)
				if err := db.AutoMigrate(tc.model); err != nil {
					t.Fatalf("repeated AutoMigrate: %v", err)
				}
				var got string
				if err := db.Raw(`SELECT d.description
FROM pg_catalog.pg_description d
JOIN pg_catalog.pg_class c ON c.oid = d.objoid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = CURRENT_SCHEMA() AND c.relname = ? AND d.objsubid = 0`, tc.indexName).Scan(&got).Error; err != nil {
					t.Fatalf("read index comment: %v", err)
				}
				if got != tc.want {
					t.Errorf("index comment = %q, want %q", got, tc.want)
				}
			})
		}
	})
}
