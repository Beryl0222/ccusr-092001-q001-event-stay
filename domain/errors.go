package domain

import "fmt"

// NotFoundError 标识资源不存在。
type NotFoundError struct {
	What string
	ID   string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("%s不存在: %s", e.What, e.ID) }

// ValidationError 标识命令输入不满足领域约定。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func validationError(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}
