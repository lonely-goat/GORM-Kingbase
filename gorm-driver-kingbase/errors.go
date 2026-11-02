package kingbase

import (
	"errors"

	"gorm.io/gorm"
	"kingbase.com/gokb"
)

var translatedErrorCodes = map[string]error{
	"23505": gorm.ErrDuplicatedKey,
	"23503": gorm.ErrForeignKeyViolated,
	"23514": gorm.ErrCheckConstraintViolated,
	"42703": gorm.ErrInvalidField,
}

// Translate 将 gokb 暴露的稳定 SQLSTATE 转成 GORM 统一错误。
// 两种兼容模式共享 KingbaseES 服务端约束错误码，因此这里无需按模式解析错误文本。
func (d *Dialector) Translate(err error) error {
	var kbError *gokb.Error
	if errors.As(err, &kbError) {
		if translated, ok := translatedErrorCodes[string(kbError.Code)]; ok {
			return translated
		}
	}
	return err
}
