package notes

import "testing"

func TestAnalyze(t *testing.T) {
	for _, test := range []struct {
		text    string
		trigger string
		want    State
	}{
		{"#bobik", "", State{Ready: true}},
		{"a note\n(#bobik: request)", "", State{Ready: true}},
		{"hey #bobik!", "", State{Ready: true}},
		{"(#bobik), please", "", State{Ready: true}},
		{"#bobik\u00a0request", "", State{Ready: true}},
		{"#bobik\u0085request", "", State{}},
		{"@bobik #Bobik #bobik-draft #bobik/foo é#bobik Ⅳ#bobik ##bobik #bobik_other", "", State{}},
		{"#bobik#bobik", "", State{}},
		{"#bobiké #bobik2 #bobik\u0301", "", State{}},
		{"%%#bobik: drafting%%", "", State{Draft: true}},
		{"%%hey #bobik!%%", "", State{Draft: true}},
		{"%%\n#bobik: drafting\n%%", "", State{Draft: true}},
		{"%%#bobik: unfinished", "", State{Draft: true}},
		{"#bobik: ready\n%% #bobik: draft %%", "", State{Draft: true}},
		{"%% other %%\n%% #bobik: draft %%", "", State{Draft: true}},
		{"`#bobik`", "", State{}},
		{"`example`#bobik: ready", "", State{Ready: true}},
		{"``#bobik: ` example``", "", State{}},
		{"\\` #bobik: ready", "", State{Ready: true}},
		{"` unmatched #bobik: ready", "", State{Ready: true}},
		{"```md\n#bobik: example\n```", "", State{}},
		{"~~~ #bobik: in info", "", State{}},
		{"~~~\n#bobik: example\n~~~", "", State{}},
		{"```\n#bobik: unfinished fence", "", State{}},
		{"%% `#bobik` %%", "", State{}},
		{"`%%` #bobik: ready", "", State{Ready: true}},
		{"```\n%%\n```\n#bobik: ready", "", State{Ready: true}},
		{"%% #bobik: draft `%%`", "", State{Draft: true}},
		{"> ```md\n> #bobik: example\n> ```", "", State{}},
		{"- ```md\n  #bobik: example\n  ```", "", State{}},
		{"    #bobik: indented example", "", State{}},
		{"> ```\n> %%\n> ```\n#bobik: ready", "", State{Ready: true}},
		{"> ``` %%\r\n> code\r\n> ```\r\n#bobik: ready", "", State{Ready: true}},
		{"> ~~~ %%\r> code\r> ~~~\r#bobik: ready", "", State{Ready: true}},
		{"> `multi\r\n> line %%` #bobik: ready", "", State{Ready: true}},
		{"> ```\n> %%\n#bobik: ready", "", State{Ready: true}},
		{"%% unrelated %% #bobik: ready", "", State{Ready: true}},
		{"````\n```\n#bobik\n````", "", State{}},
		{"prefix@custom-suffix", "@custom", State{Ready: true}},
		{"%% @custom drafting %%", "@custom", State{Draft: true}},
		{"`@custom`", "@custom", State{}},
		{"`x`", "`", State{}},
		{"```\n```", "```", State{}},
		{"%%#bobik: default%%", "@custom", State{}},
	} {
		t.Run(test.text, func(t *testing.T) {
			if got := Analyze(test.text, test.trigger); got != test.want {
				t.Errorf("Analyze = %+v, want %+v", got, test.want)
			}
		})
	}
}

func FuzzAnalyze(f *testing.F) {
	for _, seed := range []string{"```\n%%\n```\n#bobik", "> `a\n> b` #bobik", "%%#bobik%%", "#bobik", "~"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := Analyze(input, DefaultTrigger)
		if got.Ready && got.Draft {
			t.Fatal("ready and draft cannot both be true")
		}
		_ = Analyze(input, "%")
	})
}
