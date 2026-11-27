package richtext

import "testing"

func TestEscapeDollars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no dollar sign fast path",
			in:   "Обычный текст **жирный** и *курсив*",
			want: "Обычный текст **жирный** и *курсив*",
		},
		{
			name: "life-agent report example",
			in:   "Маск пообещал по 2 акции SpaceX (~$18,4 млрд) каждому ребёнку в США. **Huawei** показала складной **Mate X8** (от ~$1950 с 4.12)... Syncere (a16z) поднял $11M на робота-уборщика **LUME** в корпусе торшера ($2699)",
			want: `Маск пообещал по 2 акции SpaceX (~\$18,4 млрд) каждому ребёнку в США. **Huawei** показала складной **Mate X8** (от ~\$1950 с 4.12)... Syncere (a16z) поднял \$11M на робота-уборщика **LUME** в корпусе торшера (\$2699)`,
		},
		{
			name: "dollar sign after digits",
			in:   "100$ и 200$",
			want: `100\$ и 200\$`,
		},
		{
			name: "single price",
			in:   "Стоит $100",
			want: `Стоит \$100`,
		},
		{
			name: "pair of prices",
			in:   "От $50 до $100",
			want: `От \$50 до \$100`,
		},
		{
			name: "pair of prices without cyrillic",
			in:   "Price $50+$100 total",
			want: `Price \$50+\$100 total`,
		},
		{
			name: "mixed price and formula",
			in:   "Цена $100, формула $x + y = z$",
			want: `Цена \$100, формула $x + y = z$`,
		},
		{
			name: "mixed english price and formula",
			in:   "Price $100, formula $x + y = z$",
			want: `Price \$100, formula $x + y = z$`,
		},
		{
			name: "valid inline formulas untouched",
			in:   "Формулы: $E = mc^2$, $2 + 2 = 4$, $10^6$, $\\alpha + \\beta$",
			want: "Формулы: $E = mc^2$, $2 + 2 = 4$, $10^6$, $\\alpha + \\beta$",
		},
		{
			name: "block math untouched",
			in:   "Блок:\n$$x^2 + y^2$$\nЕщё:\n$$\nE = mc^2\n$$",
			want: "Блок:\n$$x^2 + y^2$$\nЕщё:\n$$\nE = mc^2\n$$",
		},
		{
			name: "already escaped dollar not doubled",
			in:   `Цена \$100 и \$200`,
			want: `Цена \$100 и \$200`,
		},
		{
			name: "escaped backslash before dollar is escaped",
			in:   `Путь \\$100`,
			want: `Путь \\\$100`,
		},
		{
			name: "inline code span untouched",
			in:   "Команда `echo $HOME $USER` и цена $10",
			want: "Команда `echo $HOME $USER` и цена \\$10",
		},
		{
			name: "fenced code block untouched",
			in: "```sh\n" +
				"echo $HOME $USER\n" +
				"```\n" +
				"Итого $50 и $100",
			want: "```sh\n" +
				"echo $HOME $USER\n" +
				"```\n" +
				`Итого \$50 и \$100`,
		},
		{
			name: "indented code block untouched",
			in: "Пример:\n\n" +
				"    echo $HOME $USER\n\n" +
				"Итого $10",
			want: "Пример:\n\n" +
				"    echo $HOME $USER\n\n" +
				`Итого \$10`,
		},
		{
			name: "link text escaped while url untouched",
			in:   "[цена $10](https://example.com/$1/$2)",
			want: `[цена \$10](https://example.com/$1/$2)`,
		},
		{
			name: "table cells escaped",
			in: "| Товар | Цена |\n" +
				"| --- | --- |\n" +
				"| A | $10 |\n" +
				"| B | $20 |",
			want: "| Товар | Цена |\n" +
				"| --- | --- |\n" +
				"| A | \\$10 |\n" +
				"| B | \\$20 |",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EscapeDollars(tc.in)
			if got != tc.want {
				t.Fatalf("EscapeDollars:\ngot:  %q\nwant: %q", got, tc.want)
			}
			// Идемпотентность: повторный прогон ничего не меняет.
			if again := EscapeDollars(got); again != got {
				t.Fatalf("не идемпотентна:\nfirst:  %q\nsecond: %q", got, again)
			}
		})
	}
}
