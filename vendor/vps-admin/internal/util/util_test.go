package util

import "testing"

func TestUsernameFromDisplay(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Вася Пупкин", "vasya-pupkin"},
		{"  Вася   Пупкин  ", "vasya-pupkin"},
		{"Щукин Ёж", "shchukin-ezh"},
		{"Алексей-2", "aleksey-2"},
		{"John Doe", "john-doe"},
		{"Мария О'Нейл", "mariya-o-neyl"},
		{"!!!", "user"},
		{"", "user"},
		{"Я", "ya"},
	}
	for _, c := range cases {
		if got := UsernameFromDisplay(c.in); got != c.want {
			t.Errorf("UsernameFromDisplay(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// длина ограничена 36 (запас под суффикс, лимит логина 40)
	long := UsernameFromDisplay("Очень Длинное Имя Пользователя Которое Не Помещается В Лимит")
	if len(long) > 36 {
		t.Errorf("длина %d > 36: %q", len(long), long)
	}
}
