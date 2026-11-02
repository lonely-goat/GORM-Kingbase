package kingbase

// V9R1C10 may leave EXTRA NULL for AUTO_INCREMENT while pg_attribute carries
// a nonempty attidentity ("i" on the tested MySQL instance). Keep EXTRA support
// and consult the underlying identity marker so generated keys remain visible.
const mysqlColumnMetadataSQL = `
SELECT c.column_name, c.data_type,
       CASE WHEN c.character_maximum_length = -1
                 AND c.data_type IN ('varchar', 'character varying', 'char', 'character')
                 AND a.atttypmod > 4
            THEN a.atttypmod - 4
            ELSE c.character_maximum_length END,
       c.numeric_precision, c.numeric_scale, c.datetime_precision,
       c.column_default, c.is_nullable,
       CASE WHEN a.attidentity <> '' THEN 'auto_increment' ELSE COALESCE(c.extra, '') END,
       d.description
FROM information_schema.columns AS c
LEFT JOIN pg_catalog.pg_namespace AS n ON n.nspname = c.table_schema
LEFT JOIN pg_catalog.pg_class AS t ON t.relnamespace = n.oid AND t.relname = c.table_name
LEFT JOIN pg_catalog.pg_attribute AS a ON a.attrelid = t.oid AND a.attname = c.column_name AND a.attnum > 0
LEFT JOIN pg_catalog.pg_description AS d ON d.objoid = t.oid AND d.objsubid = c.ordinal_position
WHERE c.table_schema = ? AND c.table_name = ?
ORDER BY c.ordinal_position`
