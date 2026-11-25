package richtext

import "testing"

func TestPreserveBreaks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty and single line",
			in:   "Привет",
			want: "Привет",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "em-dash vertical list after intro",
			in: "Что уже готово:\n" +
				"— авторизация и сессии\n" +
				"— приём вебхуков\n" +
				"— тесты в CI",
			want: "Что уже готово:  \n" +
				"— авторизация и сессии  \n" +
				"— приём вебхуков  \n" +
				"— тесты в CI",
		},
		{
			name: "en-dash vertical list",
			in:   "План:\n– шаг 1\n– шаг 2",
			want: "План:  \n– шаг 1  \n– шаг 2",
		},
		{
			name: "multiline paragraphs and blank lines",
			in:   "Строка 1\nСтрока 2\n\nАбзац 2 строка 1\nАбзац 2 строка 2\n",
			want: "Строка 1  \nСтрока 2\n\nАбзац 2 строка 1  \nАбзац 2 строка 2\n",
		},
		{
			name: "CRLF and bare CR normalization",
			in:   "Первая\r\nВторая\rТретья\r\n\r\nЧетвёртая\r\nПятая",
			want: "Первая  \nВторая  \nТретья\n\nЧетвёртая  \nПятая",
		},
		{
			name: "existing hard breaks preserved without doubling",
			in:   "С пробелами  \nСо слэшем\\\nЭкранированный слэш\\\\\nОбычная",
			want: "С пробелами  \nСо слэшем\\\nЭкранированный слэш\\\\  \nОбычная",
		},
		{
			name: "trailing single space and tab before newline",
			in:   "Один пробел \nТаб\t\nФинал",
			want: "Один пробел   \nТаб\t  \nФинал",
		},
		{
			name: "fenced code block backticks untouched including em-dash",
			in: "Пример:\n" +
				"```go\n" +
				"// — не трогать\n" +
				"x := 1\n" +
				"y := 2\n" +
				"```\n" +
				"После кода\n" +
				"Вторая строка",
			want: "Пример:\n" +
				"```go\n" +
				"// — не трогать\n" +
				"x := 1\n" +
				"y := 2\n" +
				"```\n" +
				"После кода  \n" +
				"Вторая строка",
		},
		{
			name: "fenced code block inside list item",
			in: "- ```go\n" +
				"  x := 1\n" +
				"  y := 2\n" +
				"  ```\n" +
				"\n" +
				"Первая\n" +
				"Вторая",
			want: "- ```go\n" +
				"  x := 1\n" +
				"  y := 2\n" +
				"  ```\n" +
				"\n" +
				"Первая  \n" +
				"Вторая",
		},
		{
			name: "indented code block untouched",
			in: "Пример:\n" +
				"\n" +
				"    x := 1\n" +
				"    y := 2\n" +
				"\n" +
				"После\n" +
				"Ещё",
			want: "Пример:\n" +
				"\n" +
				"    x := 1\n" +
				"    y := 2\n" +
				"\n" +
				"После  \n" +
				"Ещё",
		},
		{
			name: "multiline inline code span untouched",
			in:   "Смотри `foo\nbar` здесь\nВторая строка",
			want: "Смотри `foo\nbar` здесь  \nВторая строка",
		},
		{
			name: "fenced code block tildes and nested backticks",
			in: "   ~~~markdown\n" +
				"```\n" +
				"— внутри\n" +
				"— ещё строка\n" +
				"```\n" +
				"   ~~~~  \n" +
				"Строка 1\n" +
				"Строка 2",
			want: "   ~~~markdown\n" +
				"```\n" +
				"— внутри\n" +
				"— ещё строка\n" +
				"```\n" +
				"   ~~~~  \n" +
				"Строка 1  \n" +
				"Строка 2",
		},
		{
			name: "four backticks fence containing three backticks",
			in: "````\n" +
				"```\n" +
				"a\n" +
				"b\n" +
				"```\n" +
				"````\n" +
				"c\n" +
				"d",
			want: "````\n" +
				"```\n" +
				"a\n" +
				"b\n" +
				"```\n" +
				"````\n" +
				"c  \n" +
				"d",
		},
		{
			name: "inline triple backtick code span is not a fence",
			in:   "```inline```\nвторая строка",
			want: "```inline```  \nвторая строка",
		},
		{
			name: "fenced code block inside blockquote",
			in: "> Код:\n" +
				"> ```\n" +
				"> — строка 1\n" +
				"> — строка 2\n" +
				"> ```\n" +
				"> После кода\n" +
				"> Ещё строка",
			want: "> Код:\n" +
				"> ```\n" +
				"> — строка 1\n" +
				"> — строка 2\n" +
				"> ```\n" +
				"> После кода  \n" +
				"> Ещё строка",
		},
		{
			name: "fenced code block inside blockquote with tab after gt",
			in: ">\t```go\n" +
				">\tx := 1\n" +
				">\ty := 2\n" +
				">\t```\n" +
				"\n" +
				"Первая\n" +
				"Вторая",
			want: ">\t```go\n" +
				">\tx := 1\n" +
				">\ty := 2\n" +
				">\t```\n" +
				"\n" +
				"Первая  \n" +
				"Вторая",
		},
		{
			name: "unclosed fenced code block inside blockquote ends with quote",
			in: "> ```\n" +
				"> строка кода\n" +
				"обычная 1\n" +
				"обычная 2",
			want: "> ```\n" +
				"> строка кода\n" +
				"обычная 1  \n" +
				"обычная 2",
		},
		{
			name: "blockquote lazy continuation preserves breaks",
			in: "> первая\n" +
				"вторая\n" +
				"> третья",
			want: "> первая  \n" +
				"вторая  \n" +
				"> третья",
		},
		{
			name: "ordered list starting at 2 does not interrupt paragraph",
			in:   "Готово\n2. В работе",
			want: "Готово  \n2. В работе",
		},
		{
			name: "multiline setext heading untouched",
			in:   "Первая\nВторая\n===",
			want: "Первая\nВторая\n===",
		},
		{
			name: "GFM table with outer pipes untouched",
			in: "Таблица:\n" +
				"\n" +
				"| Поле | Значение |\n" +
				"| --- | ---: |\n" +
				"| A | 1 |\n" +
				"| B | 2 |\n" +
				"\n" +
				"После таблицы\n" +
				"Вторая строка",
			want: "Таблица:\n" +
				"\n" +
				"| Поле | Значение |\n" +
				"| --- | ---: |\n" +
				"| A | 1 |\n" +
				"| B | 2 |\n" +
				"\n" +
				"После таблицы  \n" +
				"Вторая строка",
		},
		{
			name: "GFM table without outer pipes untouched",
			in: "Колонка 1 | Колонка 2\n" +
				"--- | ---\n" +
				"val 1 | val 2\n" +
				"val 3 | val 4",
			want: "Колонка 1 | Колонка 2\n" +
				"--- | ---\n" +
				"val 1 | val 2\n" +
				"val 3 | val 4",
		},
		{
			name: "GFM single-hyphen delimiter without outer pipes",
			in:   "A | B\n-|-\n1 | 2\n\nТекст 1\nТекст 2",
			want: "A | B\n-|-\n1 | 2\n\nТекст 1  \nТекст 2",
		},
		{
			name: "hyphen-space-pipe starts a list item not a GFM table",
			in:   "A | B\n- | -\n1 | 2",
			want: "A | B\n- | -  \n1 | 2",
		},
		{
			name: "mismatched table header and delimiter cells is a paragraph",
			in: "| A | B | C |\n" +
				"| --- | --- |\n" +
				"строка 3",
			want: "| A | B | C |  \n" +
				"| --- | --- |  \n" +
				"строка 3",
		},
		{
			name: "standard lists unordered and ordered",
			in: "Список:\n" +
				"- раз\n" +
				"- два\n" +
				"* три\n" +
				"+ четыре\n" +
				"1. пять\n" +
				"2) шесть",
			want: "Список:\n" +
				"- раз\n" +
				"- два\n" +
				"* три\n" +
				"+ четыре\n" +
				"1. пять\n" +
				"2) шесть",
		},
		{
			name: "multiline list items preserve continuation breaks",
			in: "- пункт 1\n" +
				"  продолжение 1\n" +
				"  продолжение 2\n" +
				"- пункт 2\n" +
				"  - вложенный 1\n" +
				"    - глубоко вложенный\n" +
				"  - вложенный 2",
			want: "- пункт 1  \n" +
				"  продолжение 1  \n" +
				"  продолжение 2\n" +
				"- пункт 2\n" +
				"  - вложенный 1\n" +
				"    - глубоко вложенный\n" +
				"  - вложенный 2",
		},
		{
			name: "emphasis and dates are not list items",
			in:   "Встреча\n*важно*\n**очень**\n25.11.2026 в 18:00",
			want: "Встреча  \n*важно*  \n**очень**  \n25.11.2026 в 18:00",
		},
		{
			name: "ATX headings and hashtag",
			in: "# Заголовок\n" +
				"Текст 1\n" +
				"## Подзаголовок\n" +
				"Текст 2\n" +
				"#тег\n" +
				"Текст 3",
			want: "# Заголовок\n" +
				"Текст 1\n" +
				"## Подзаголовок\n" +
				"Текст 2  \n" +
				"#тег  \n" +
				"Текст 3",
		},
		{
			name: "thematic breaks and setext headings",
			in: "Заголовок\n" +
				"===\n" +
				"Строка 1\n" +
				"Строка 2\n" +
				"---\n" +
				"Строка 3\n" +
				"***\n" +
				"Строка 4\n" +
				"___\n" +
				"Строка 5",
			want: "Заголовок\n" +
				"===\n" +
				"Строка 1\n" +
				"Строка 2\n" +
				"---\n" +
				"Строка 3\n" +
				"***\n" +
				"Строка 4\n" +
				"___\n" +
				"Строка 5",
		},
		{
			name: "blockquotes multiline and blank line separation",
			in: "До цитаты\n" +
				"> строка 1\n" +
				"> строка 2\n" +
				">\n" +
				"> абзац 2\n" +
				">> вложенная 1\n" +
				">> вложенная 2\n" +
				"\n" +
				"После цитаты\n" +
				"Ещё строка",
			want: "До цитаты\n" +
				"> строка 1  \n" +
				"> строка 2\n" +
				">\n" +
				"> абзац 2\n" +
				">> вложенная 1  \n" +
				">> вложенная 2\n" +
				"\n" +
				"После цитаты  \n" +
				"Ещё строка",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PreserveBreaks(tc.in)
			if got != tc.want {
				t.Fatalf("PreserveBreaks:\ngot:  %q\nwant: %q", got, tc.want)
			}
			// Идемпотентность: повторный прогон ничего не меняет.
			again := PreserveBreaks(got)
			if again != got {
				t.Fatalf("не идемпотентна:\nfirst:  %q\nsecond: %q", got, again)
			}
		})
	}
}
