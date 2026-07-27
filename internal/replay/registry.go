package replay

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"runtime"
	"strings"
	"sync"
)

// dictFS holds the dictionaries. This directory is the ONLY copy of them: the
// frontend no longer ships static/languages, it reads the catalogue and the
// hash-addressed bodies from this server.
//
//go:embed dicts/*.json
var dictFS embed.FS

const dictsDir = "dicts"

// CatalogueEntry is one row of GET /api/v1/dictionaries — the catalogue clients
// use to pick a language and to learn the hash its body is addressed by.
type CatalogueEntry struct {
	// Lang is the dictionary's canonical key, i.e. its file name without
	// .json. This is the ONLY thing that travels: it is what a run submission,
	// a match setting and a leaderboard bucket key all carry. Clients index on
	// it and must never render it.
	Lang string `json:"lang"`
	// Name is the human display name of the language — presentation data owned
	// by THIS catalogue, not by the dictionary file and not by the client. It
	// comes from displayNames, an explicit table (see its comment for why it is
	// a table and not a transformation of Lang), so the picker shows
	// "CSS (code)" rather than the key it sends back.
	Name string `json:"name"`
	// DictHash is the core's FNV-1a fingerprint of the word list, and the path
	// segment of the body: /static/dictionaries/{dictHash}.json.
	DictHash string `json:"dictHash"`
	// WordCount is the number of words in the list.
	WordCount int `json:"wordCount"`
	// Bytes is the exact length of the uncompressed body.
	Bytes int `json:"bytes"`
}

// entry is a seeded dictionary: its catalogue row plus the pre-built response
// bodies. Both encodings are produced once at startup — a request never
// marshals, never compresses, and never touches a file.
type entry struct {
	CatalogueEntry
	raw []byte
	gz  []byte
}

// Registry is the immutable, fully-seeded set of dictionaries. It is built once
// at startup and never mutated, so it is safe for concurrent use without a lock.
type Registry struct {
	entries []entry           // sorted by Lang (embed.FS walks in name order)
	byHash  map[string]*entry // dictHash -> body, for the static endpoint
}

// dictDoc is the on-disk dictionary shape. Only the fields the registry needs
// are modelled; the body is served verbatim, so extras (bcp47, rightToleft…)
// travel to the client untouched.
type dictDoc struct {
	Name  string   `json:"name"`
	Words []string `json:"words"`
}

// displayNames maps a canonical language key to the human name the catalogue
// publishes. It is an explicit table on purpose, and the two halves of that
// purpose are worth stating separately.
//
// It is explicit because a display name is NOT derivable. "code_css" is spoken
// "CSS (code)", "arabian" is spoken "Arabic", "russian_empire" is spoken
// "Russian (pre-reform)" and "chinese" is spoken "Chinese (simplified)".
// Title-casing the key and swapping underscores for spaces produces "Code Css",
// "Arabian" and "Russian Empire" — three wrong answers dressed as an algorithm.
// Serving the key raw is the same bug with the disguise removed: it is how a
// language picker came to offer the user "css_code".
//
// It lives HERE because the name is presentation data owned by the server, and
// the catalogue is the server's one statement about a language. The dictionary
// FILE also carries a `name`, but that is the bundle's `dictName` — an internal
// corpus label ("russian_1k", "arabic") that is either the key verbatim or a
// vendoring detail. Reading it as a display name is what put raw keys in the UI.
//
// The key naming rule this table encodes (plain languages plain, every code
// dictionary in the code_<lang> family) is documented in docs/DICTIONARIES.md.
// A MISSING row is a startup error — see displayName — so this table has one
// row per embedded dictionary and TestEveryDictionaryHasADisplayName holds it
// to that in both directions.
//
// Size variants are named from their base ("Polish (200k)", "Python (code,
// 5k)"), which is a decoration and not a derivation: the base name is still
// authored. That distinction is the whole point of the table — no rule turns
// "code_cpp" into "C++", "euskera" into "Basque" or "myanmar_burmese" into
// "Burmese", and a rule that produced "Code Cpp" would look like it worked.
var displayNames = map[string]string{
	"afrikaans":                    "Afrikaans",
	"afrikaans_10k":                "Afrikaans (10k)",
	"afrikaans_1k":                 "Afrikaans (1k)",
	"albanian":                     "Albanian",
	"albanian_1k":                  "Albanian (1k)",
	"amharic":                      "Amharic",
	"amharic_1k":                   "Amharic (1k)",
	"amharic_5k":                   "Amharic (5k)",
	"arabian":                      "Arabic",
	"arabian_10k":                  "Arabic (10k)",
	"arabian_egypt":                "Arabic (Egyptian)",
	"arabian_egypt_1k":             "Arabic (Egyptian, 1k)",
	"arabian_morocco":              "Arabic (Moroccan)",
	"armenian":                     "Armenian",
	"armenian_1k":                  "Armenian (1k)",
	"armenian_western":             "Armenian (Western)",
	"armenian_western_1k":          "Armenian (Western, 1k)",
	"azerbaijani":                  "Azerbaijani",
	"azerbaijani_1k":               "Azerbaijani (1k)",
	"bangla":                       "Bangla",
	"bangla_10k":                   "Bangla (10k)",
	"bangla_letters":               "Bangla (letters)",
	"bashkir":                      "Bashkir",
	"belarusian":                   "Belarusian",
	"belarusian_100k":              "Belarusian (100k)",
	"belarusian_10k":               "Belarusian (10k)",
	"belarusian_1k":                "Belarusian (1k)",
	"belarusian_25k":               "Belarusian (25k)",
	"belarusian_50k":               "Belarusian (50k)",
	"belarusian_5k":                "Belarusian (5k)",
	"belarusian_lacinka":           "Belarusian (Łacinka)",
	"belarusian_lacinka_1k":        "Belarusian (Łacinka, 1k)",
	"bemba":                        "Bemba",
	"bemba_10k":                    "Bemba (10k)",
	"bemba_1k":                     "Bemba (1k)",
	"bosnian":                      "Bosnian",
	"bosnian_4k":                   "Bosnian (4k)",
	"bulgarian":                    "Bulgarian",
	"bulgarian_1k":                 "Bulgarian (1k)",
	"bulgarian_latin":              "Bulgarian (Latin)",
	"bulgarian_latin_1k":           "Bulgarian (Latin, 1k)",
	"catalan":                      "Catalan",
	"catalan_1k":                   "Catalan (1k)",
	"chinese":                      "Chinese (simplified)",
	"chinese_10k":                  "Chinese (simplified, 10k)",
	"chinese_1k":                   "Chinese (simplified, 1k)",
	"chinese_50k":                  "Chinese (simplified, 50k)",
	"chinese_5k":                   "Chinese (simplified, 5k)",
	"code_6502_assembly":           "6502 assembly (code)",
	"code_abap":                    "ABAP (code)",
	"code_abap_1k":                 "ABAP (code, 1k)",
	"code_arduino":                 "Arduino (code)",
	"code_assembly":                "Assembly (code)",
	"code_bash":                    "Bash (code)",
	"code_c":                       "C (code)",
	"code_clojure":                 "Clojure (code)",
	"code_cobol":                   "COBOL (code)",
	"code_cpp":                     "C++ (code)",
	"code_csharp":                  "C# (code)",
	"code_css":                     "CSS (code)",
	"code_dart":                    "Dart (code)",
	"code_elixir":                  "Elixir (code)",
	"code_erlang":                  "Erlang (code)",
	"code_fortran":                 "Fortran (code)",
	"code_fsharp":                  "F# (code)",
	"code_gdscript":                "GDScript (code)",
	"code_gdscript_2":              "GDScript 2 (code)",
	"code_go":                      "Go (code)",
	"code_haskell":                 "Haskell (code)",
	"code_html":                    "HTML (code)",
	"code_java":                    "Java (code)",
	"code_javascript":              "JavaScript (code)",
	"code_javascript_1k":           "JavaScript (code, 1k)",
	"code_javascript_react":        "JavaScript React (code)",
	"code_jule":                    "Jule (code)",
	"code_julia":                   "Julia (code)",
	"code_kotlin":                  "Kotlin (code)",
	"code_lua":                     "Lua (code)",
	"code_luau":                    "Luau (code)",
	"code_matlab":                  "MATLAB (code)",
	"code_nim":                     "Nim (code)",
	"code_nix":                     "Nix (code)",
	"code_ocaml":                   "OCaml (code)",
	"code_odin":                    "Odin (code)",
	"code_ook":                     "Ook! (code)",
	"code_opencl":                  "OpenCL (code)",
	"code_pascal":                  "Pascal (code)",
	"code_perl":                    "Perl (code)",
	"code_php":                     "PHP (code)",
	"code_python":                  "Python (code)",
	"code_python_1k":               "Python (code, 1k)",
	"code_python_2k":               "Python (code, 2k)",
	"code_python_5k":               "Python (code, 5k)",
	"code_r":                       "R (code)",
	"code_r_2k":                    "R (code, 2k)",
	"code_rockstar":                "Rockstar (code)",
	"code_ruby":                    "Ruby (code)",
	"code_rust":                    "Rust (code)",
	"code_scala":                   "Scala (code)",
	"code_sql":                     "SQL (code)",
	"code_swift":                   "Swift (code)",
	"code_systemverilog":           "SystemVerilog (code)",
	"code_typescript":              "TypeScript (code)",
	"code_typst":                   "Typst (code)",
	"code_v":                       "V (code)",
	"code_vhdl":                    "VHDL (code)",
	"code_vim":                     "Vim (code)",
	"code_vimscript":               "Vimscript (code)",
	"code_visual_basic":            "Visual Basic (code)",
	"code_yoptascript":             "YoptaScript (code)",
	"code_zig":                     "Zig (code)",
	"croatian":                     "Croatian",
	"croatian_1k":                  "Croatian (1k)",
	"czech":                        "Czech",
	"czech_10k":                    "Czech (10k)",
	"czech_1k":                     "Czech (1k)",
	"danish":                       "Danish",
	"danish_10k":                   "Danish (10k)",
	"danish_1k":                    "Danish (1k)",
	"docker_file":                  "Dockerfile (code)",
	"dutch":                        "Dutch",
	"dutch_1k":                     "Dutch (1k)",
	"english":                      "English",
	"english_10k":                  "English (10k)",
	"english_1k":                   "English (1k)",
	"english_25k":                  "English (25k)",
	"english_5k":                   "English (5k)",
	"english_commonly_misspelled":  "English (commonly misspelled)",
	"english_contractions":         "English (contractions)",
	"english_doubleletter":         "English (double letters)",
	"english_medical":              "English (medical)",
	"english_old":                  "Old English",
	"english_shakespearean":        "English (Shakespearean)",
	"esperanto":                    "Esperanto",
	"esperanto_10k":                "Esperanto (10k)",
	"esperanto_1k":                 "Esperanto (1k)",
	"esperanto_25k":                "Esperanto (25k)",
	"esperanto_36k":                "Esperanto (36k)",
	"esperanto_h_sistemo":          "Esperanto (h-sistemo)",
	"esperanto_h_sistemo_10k":      "Esperanto (h-sistemo, 10k)",
	"esperanto_h_sistemo_1k":       "Esperanto (h-sistemo, 1k)",
	"esperanto_h_sistemo_25k":      "Esperanto (h-sistemo, 25k)",
	"esperanto_h_sistemo_36k":      "Esperanto (h-sistemo, 36k)",
	"esperanto_x_sistemo":          "Esperanto (x-sistemo)",
	"esperanto_x_sistemo_10k":      "Esperanto (x-sistemo, 10k)",
	"esperanto_x_sistemo_1k":       "Esperanto (x-sistemo, 1k)",
	"esperanto_x_sistemo_25k":      "Esperanto (x-sistemo, 25k)",
	"esperanto_x_sistemo_36k":      "Esperanto (x-sistemo, 36k)",
	"estonian":                     "Estonian",
	"estonian_10k":                 "Estonian (10k)",
	"estonian_1k":                  "Estonian (1k)",
	"estonian_5k":                  "Estonian (5k)",
	"euskera":                      "Basque",
	"filipino":                     "Filipino",
	"filipino_1k":                  "Filipino (1k)",
	"finnish":                      "Finnish",
	"finnish_10k":                  "Finnish (10k)",
	"finnish_1k":                   "Finnish (1k)",
	"french":                       "French",
	"french_10k":                   "French (10k)",
	"french_1k":                    "French (1k)",
	"french_2k":                    "French (2k)",
	"french_bitoduc":               "French (Bitoduc)",
	"frisian":                      "Frisian",
	"frisian_1k":                   "Frisian (1k)",
	"friulian":                     "Friulian",
	"galician":                     "Galician",
	"georgian":                     "Georgian",
	"german":                       "German",
	"german_10k":                   "German (10k)",
	"german_1k":                    "German (1k)",
	"git":                          "Git (code)",
	"greek":                        "Greek",
	"greek_10k":                    "Greek (10k)",
	"greek_1k":                     "Greek (1k)",
	"greek_25k":                    "Greek (25k)",
	"greek_5k":                     "Greek (5k)",
	"greek_koine":                  "Greek (Koine)",
	"greeklish":                    "Greeklish",
	"greeklish_10k":                "Greeklish (10k)",
	"greeklish_1k":                 "Greeklish (1k)",
	"greeklish_25k":                "Greeklish (25k)",
	"greeklish_5k":                 "Greeklish (5k)",
	"gujarati":                     "Gujarati",
	"gujarati_1k":                  "Gujarati (1k)",
	"hausa":                        "Hausa",
	"hausa_1k":                     "Hausa (1k)",
	"hawaiian":                     "Hawaiian",
	"hawaiian_1k":                  "Hawaiian (1k)",
	"hebrew":                       "Hebrew",
	"hebrew_10k":                   "Hebrew (10k)",
	"hebrew_1k":                    "Hebrew (1k)",
	"hebrew_5k":                    "Hebrew (5k)",
	"hindi":                        "Hindi",
	"hindi_1k":                     "Hindi (1k)",
	"hinglish":                     "Hinglish",
	"hungarian":                    "Hungarian",
	"hungarian_1k":                 "Hungarian (1k)",
	"hungarian_2k":                 "Hungarian (2k)",
	"icelandic":                    "Icelandic",
	"icelandic_1k":                 "Icelandic (1k)",
	"indonesian":                   "Indonesian",
	"indonesian_10k":               "Indonesian (10k)",
	"indonesian_1k":                "Indonesian (1k)",
	"irish":                        "Irish",
	"irish_1k":                     "Irish (1k)",
	"italian":                      "Italian",
	"italian_1k":                   "Italian (1k)",
	"italian_280k":                 "Italian (280k)",
	"italian_60k":                  "Italian (60k)",
	"italian_7k":                   "Italian (7k)",
	"japanese":                     "Japanese",
	"japanese_hiragana":            "Japanese (hiragana)",
	"japanese_katakana":            "Japanese (katakana)",
	"japanese_romaji":              "Japanese (rōmaji)",
	"japanese_romaji_1k":           "Japanese (rōmaji, 1k)",
	"jyutping":                     "Jyutping",
	"kabyle":                       "Kabyle",
	"kabyle_10k":                   "Kabyle (10k)",
	"kabyle_1k":                    "Kabyle (1k)",
	"kabyle_2k":                    "Kabyle (2k)",
	"kabyle_5k":                    "Kabyle (5k)",
	"kannada":                      "Kannada",
	"kazakh":                       "Kazakh",
	"kazakh_1k":                    "Kazakh (1k)",
	"khmer":                        "Khmer",
	"kinyarwanda":                  "Kinyarwanda",
	"klingon":                      "Klingon",
	"klingon_1k":                   "Klingon (1k)",
	"kokanu":                       "kokanu",
	"korean":                       "Korean",
	"korean_1k":                    "Korean (1k)",
	"korean_5k":                    "Korean (5k)",
	"kurdish_central":              "Kurdish (Central)",
	"kurdish_central_2k":           "Kurdish (Central, 2k)",
	"kurdish_central_4k":           "Kurdish (Central, 4k)",
	"kyrgyz":                       "Kyrgyz",
	"kyrgyz_1k":                    "Kyrgyz (1k)",
	"lao":                          "Lao",
	"latin":                        "Latin",
	"latvian":                      "Latvian",
	"latvian_1k":                   "Latvian (1k)",
	"league_of_legends":            "League of Legends",
	"likanu":                       "likanu",
	"lithuanian":                   "Lithuanian",
	"lithuanian_1k":                "Lithuanian (1k)",
	"lithuanian_3k":                "Lithuanian (3k)",
	"lojban_cmavo":                 "Lojban (cmavo)",
	"lojban_gismu":                 "Lojban (gismu)",
	"lorem_ipsum":                  "Lorem ipsum",
	"macedonian":                   "Macedonian",
	"macedonian_10k":               "Macedonian (10k)",
	"macedonian_1k":                "Macedonian (1k)",
	"macedonian_75k":               "Macedonian (75k)",
	"malagasy":                     "Malagasy",
	"malagasy_1k":                  "Malagasy (1k)",
	"malay":                        "Malay",
	"malay_1k":                     "Malay (1k)",
	"malayalam":                    "Malayalam",
	"maltese":                      "Maltese",
	"maltese_1k":                   "Maltese (1k)",
	"maori_1k":                     "Māori (1k)",
	"marathi":                      "Marathi",
	"mongolian":                    "Mongolian",
	"mongolian_10k":                "Mongolian (10k)",
	"myanmar_burmese":              "Burmese",
	"nepali":                       "Nepali",
	"nepali_1k":                    "Nepali (1k)",
	"nepali_romanized":             "Nepali (romanised)",
	"norwegian_bokmal":             "Norwegian (Bokmål)",
	"norwegian_bokmal_10k":         "Norwegian (Bokmål, 10k)",
	"norwegian_bokmal_150k":        "Norwegian (Bokmål, 150k)",
	"norwegian_bokmal_1k":          "Norwegian (Bokmål, 1k)",
	"norwegian_bokmal_5k":          "Norwegian (Bokmål, 5k)",
	"norwegian_nynorsk":            "Norwegian (Nynorsk)",
	"norwegian_nynorsk_100k":       "Norwegian (Nynorsk, 100k)",
	"norwegian_nynorsk_10k":        "Norwegian (Nynorsk, 10k)",
	"norwegian_nynorsk_1k":         "Norwegian (Nynorsk, 1k)",
	"norwegian_nynorsk_5k":         "Norwegian (Nynorsk, 5k)",
	"occitan":                      "Occitan",
	"occitan_10k":                  "Occitan (10k)",
	"occitan_1k":                   "Occitan (1k)",
	"occitan_2k":                   "Occitan (2k)",
	"occitan_5k":                   "Occitan (5k)",
	"oromo":                        "Oromo",
	"oromo_1k":                     "Oromo (1k)",
	"oromo_5k":                     "Oromo (5k)",
	"pashto":                       "Pashto",
	"persian":                      "Persian",
	"persian_1k":                   "Persian (1k)",
	"persian_20k":                  "Persian (20k)",
	"persian_5k":                   "Persian (5k)",
	"persian_romanized":            "Persian (romanised)",
	"pig_latin":                    "Pig Latin",
	"pinyin":                       "Pinyin",
	"pinyin_10k":                   "Pinyin (10k)",
	"pinyin_1k":                    "Pinyin (1k)",
	"pokemon_1k":                   "Pokémon (1k)",
	"polish":                       "Polish",
	"polish_10k":                   "Polish (10k)",
	"polish_200k":                  "Polish (200k)",
	"polish_20k":                   "Polish (20k)",
	"polish_2k":                    "Polish (2k)",
	"polish_40k":                   "Polish (40k)",
	"polish_5k":                    "Polish (5k)",
	"portuguese":                   "Portuguese",
	"portuguese_1k":                "Portuguese (1k)",
	"portuguese_3k":                "Portuguese (3k)",
	"portuguese_5k":                "Portuguese (5k)",
	"portuguese_acentos_e_cedilha": "Portuguese (accents & cedilla)",
	"quenya":                       "Quenya",
	"romanian":                     "Romanian",
	"romanian_100k":                "Romanian (100k)",
	"romanian_10k":                 "Romanian (10k)",
	"romanian_1k":                  "Romanian (1k)",
	"romanian_200k":                "Romanian (200k)",
	"romanian_25k":                 "Romanian (25k)",
	"romanian_50k":                 "Romanian (50k)",
	"romanian_5k":                  "Romanian (5k)",
	"russian":                      "Russian",
	"russian_10k":                  "Russian (10k)",
	"russian_1k":                   "Russian (1k)",
	"russian_25k":                  "Russian (25k)",
	"russian_50k":                  "Russian (50k)",
	"russian_5k":                   "Russian (5k)",
	"russian_abbreviations":        "Russian (abbreviations)",
	"russian_contractions":         "Russian (contractions)",
	"russian_contractions_1k":      "Russian (contractions, 1k)",
	"russian_empire":               "Russian (pre-reform)",
	"sanskrit":                     "Sanskrit",
	"sanskrit_roman":               "Sanskrit (romanised)",
	"santali":                      "Santali",
	"serbian":                      "Serbian",
	"serbian_10k":                  "Serbian (10k)",
	"serbian_latin":                "Serbian (Latin)",
	"serbian_latin_10k":            "Serbian (Latin, 10k)",
	"shona":                        "Shona",
	"shona_1k":                     "Shona (1k)",
	"sindhi":                       "Sindhi",
	"sinhala":                      "Sinhala",
	"slovak":                       "Slovak",
	"slovak_10k":                   "Slovak (10k)",
	"slovak_1k":                    "Slovak (1k)",
	"slovenian":                    "Slovenian",
	"slovenian_1k":                 "Slovenian (1k)",
	"slovenian_5k":                 "Slovenian (5k)",
	"spanish":                      "Spanish",
	"spanish_10k":                  "Spanish (10k)",
	"spanish_1k":                   "Spanish (1k)",
	"swahili_1k":                   "Swahili (1k)",
	"swedish":                      "Swedish",
	"swedish_1k":                   "Swedish (1k)",
	"swedish_diacritics":           "Swedish (diacritics)",
	"swiss_german":                 "Swiss German",
	"swiss_german_1k":              "Swiss German (1k)",
	"swiss_german_2k":              "Swiss German (2k)",
	"tamil":                        "Tamil",
	"tamil_1k":                     "Tamil (1k)",
	"tamil_old":                    "Tamil (old)",
	"tanglish":                     "Tanglish",
	"tatar":                        "Tatar",
	"tatar_1k":                     "Tatar (1k)",
	"tatar_5k":                     "Tatar (5k)",
	"tatar_9k":                     "Tatar (9k)",
	"tatar_crimean":                "Crimean Tatar",
	"tatar_crimean_10k":            "Crimean Tatar (10k)",
	"tatar_crimean_15k":            "Crimean Tatar (15k)",
	"tatar_crimean_1k":             "Crimean Tatar (1k)",
	"tatar_crimean_5k":             "Crimean Tatar (5k)",
	"traditional_chinese":          "Chinese (traditional)",
}

// displayName resolves a dictionary's catalogue name, or fails.
//
// There is deliberately no fallback. Falling back to the key is not a
// degradation, it is the original bug: it puts "code_css" in front of a user
// and looks like a working catalogue while doing it. Failing here instead means
// vendoring a dictionary without naming it cannot start the server, which turns
// a silent UI regression into a build-time omission the author sees at once.
func displayName(lang string) (string, error) {
	name, ok := displayNames[lang]
	if !ok {
		return "", fmt.Errorf(
			"replay: dictionary %q has no display name: add a row to displayNames in registry.go "+
				"(the catalogue owns the human name; it is never derived from the key)", lang)
	}
	return name, nil
}

// NewRegistry seeds the registry from the embedded dictionaries, computing every
// fingerprint through the goja bundle. Any problem — unparseable file, empty
// word list, a language with no display name, or two dictionaries hashing to
// the same value — is a startup error: a half-seeded catalogue would hand
// clients a language they cannot fetch, a hash that resolves to the wrong
// words, or a row the picker can only render as a raw key.
func NewRegistry(core *Core) (*Registry, error) {
	files, err := fs.ReadDir(dictFS, dictsDir)
	if err != nil {
		return nil, fmt.Errorf("replay: read dictionaries: %w", err)
	}

	names := make([]string, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		names = append(names, f.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("replay: no dictionaries embedded from %s/", dictsDir)
	}

	loaded, err := seed(core, names)
	if err != nil {
		return nil, err
	}
	reg := &Registry{entries: loaded, byHash: make(map[string]*entry, len(loaded))}

	// Index after the slice is final — earlier pointers would dangle on growth.
	for i := range reg.entries {
		e := &reg.entries[i]
		if prev, dup := reg.byHash[e.DictHash]; dup {
			return nil, fmt.Errorf(
				"replay: dictionaries %q and %q share dict_hash %s: content addressing requires distinct word lists",
				prev.Lang, e.Lang, e.DictHash)
		}
		reg.byHash[e.DictHash] = e
	}
	return reg, nil
}

// seed loads every dictionary in parallel, preserving the order of names.
//
// It is parallel because the corpus stopped being small. At 439 dictionaries /
// 57 MB the serial cost is 31 s — 22 s hashing in goja and 9 s gzipping — which
// is startup latency on every deploy and every test binary that builds a
// registry. Both halves are per-file and share nothing, so the work divides
// cleanly; measured on a 6-core/12-thread box the same seed takes ~3.4 s.
//
// Each worker gets its OWN Core, because a goja runtime is single-threaded and
// Core serialises on a mutex — sharing one would hand the whole pool back its
// serial cost. That is affordable only because building a Core is cheap next to
// using one: NewCore is ~8 ms (it compiles the bundle once), against ~50 ms of
// hashing per worker share. The hash still comes from the vendored bundle and
// from nowhere else; what is parallel is how many copies of that bundle run at
// once, not what any of them computes.
func seed(core *Core, names []string) ([]entry, error) {
	workers := runtime.GOMAXPROCS(0)
	if workers > len(names) {
		workers = len(names)
	}
	if workers < 1 {
		workers = 1
	}

	out := make([]entry, len(names))
	errs := make([]error, workers)
	next := make(chan int)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Worker 0 reuses the caller's Core so the single-dictionary case
			// costs exactly what it always did.
			c := core
			if w > 0 {
				var err error
				if c, err = NewCore(core.timeout); err != nil {
					errs[w] = fmt.Errorf("replay: seed worker %d: %w", w, err)
				}
			}
			// A failed worker keeps draining and does no work: the producer
			// below is an unbuffered send per file, so a worker that returned
			// early would deadlock the seed instead of failing it.
			for i := range next {
				if errs[w] != nil {
					continue
				}
				e, err := loadDict(c, names[i])
				if err != nil {
					errs[w] = err
					continue
				}
				out[i] = e
			}
		}(w)
	}
	for i := range names {
		next <- i
	}
	close(next)
	wg.Wait()

	// Report the first failure in worker order so the message is stable across
	// runs; a half-seeded catalogue is a startup error either way.
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadDict(core *Core, fileName string) (entry, error) {
	lang := strings.TrimSuffix(fileName, ".json")
	raw, err := dictFS.ReadFile(path.Join(dictsDir, fileName))
	if err != nil {
		return entry{}, fmt.Errorf("replay: read dictionary %q: %w", lang, err)
	}

	var doc dictDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return entry{}, fmt.Errorf("replay: parse dictionary %q: %w", lang, err)
	}
	// The file's own `name` is the bundle's dictName, not a display name — the
	// catalogue's name comes from displayNames. It is still required to be
	// present, because the body is served verbatim and the core reads it.
	if doc.Name == "" {
		return entry{}, fmt.Errorf("replay: dictionary %q has no name", lang)
	}
	if len(doc.Words) == 0 {
		return entry{}, fmt.Errorf("replay: dictionary %q has no words", lang)
	}

	name, err := displayName(lang)
	if err != nil {
		return entry{}, err
	}

	hash, err := core.DictVersion(doc.Words)
	if err != nil {
		return entry{}, fmt.Errorf("replay: hash dictionary %q: %w", lang, err)
	}

	gz, err := gzipBytes(raw)
	if err != nil {
		return entry{}, fmt.Errorf("replay: compress dictionary %q: %w", lang, err)
	}

	return entry{
		CatalogueEntry: CatalogueEntry{
			Lang:      lang,
			Name:      name,
			DictHash:  hash,
			WordCount: len(doc.Words),
			Bytes:     len(raw),
		},
		raw: raw,
		gz:  gz,
	}, nil
}

// Catalogue returns the catalogue rows, ordered by language code.
func (r *Registry) Catalogue() []CatalogueEntry {
	out := make([]CatalogueEntry, len(r.entries))
	for i := range r.entries {
		out[i] = r.entries[i].CatalogueEntry
	}
	return out
}

// Body returns the exact stored bytes of the dictionary with the given hash.
// The slice is the registry's own — callers MUST NOT modify it.
func (r *Registry) Body(dictHash string) ([]byte, bool) {
	e, ok := r.byHash[dictHash]
	if !ok {
		return nil, false
	}
	return e.raw, true
}

// gzipBytes compresses at the highest level: it happens once per dictionary at
// startup, so trading CPU for a permanently smaller response is free.
func gzipBytes(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(src) / 2)
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(src); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
