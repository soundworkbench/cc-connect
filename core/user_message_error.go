package core

// UserMessageError carries an actionable message without assuming a platform or
// agent language. Logs use English; command replies use the engine's language.
type UserMessageError struct {
	Key  MsgKey
	Args []any
}

func (e *UserMessageError) Error() string { return e.Localize(LangEnglish) }
func (e *UserMessageError) Localize(lang Language) string {
	return NewI18n(lang).Tf(e.Key, e.Args...)
}
