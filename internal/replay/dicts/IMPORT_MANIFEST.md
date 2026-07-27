# Dictionary import manifest

The inventory behind the mass language import: every file in monkeytype's
`frontend/static/languages` at the pinned upstream commit, the canonical key it
is published under, and whether it was imported or skipped.

This file is the **plan**, and it is committed before the corpus so the two can
be diffed against each other. `docs/DICTIONARIES.md`'s language table is
regenerated from it.

| | |
|---|---|
| Upstream | `https://github.com/monkeytypegame/monkeytype` |
| Path | `frontend/static/languages` |
| Files inspected | 446 |
| **Imported** | **429** |
| Skipped | 17 |
| Already published (10 pre-existing) | 40.9 kB |
| **Total embedded after import** | **57.37 MB** (warn threshold 60 MB) |
| Right-to-left scripts imported | 21 |

## What a row means

- **key** — the canonical dictionary key, i.e. the vendored file's basename. It
  is what a run submission, a match setting and a leaderboard bucket key carry;
  it is never rendered. Sanitised to `^[a-z0-9_]+$`.
- **name** — the human display name published in the catalogue, from
  `displayNames` in `internal/replay/registry.go`.
- **bytes** — the exact length of the **normalised** body, which is what the
  catalogue publishes and what the size budget counts. It is smaller than the
  upstream file: see *Normalisation*.
- **words** — the length of the word list.

## Naming

Plain languages keep plain keys, including their `_1k`/`_5k`/`_10k` size
variants; code dictionaries live in the `code_<lang>` family. Both clauses are
docs/DICTIONARIES.md's naming contract, unchanged. 16 files needed a
non-trivial mapping and every one of them is journaled:

| Upstream file | Key | Why |
|---|---|---|
| `arabic.json` | `arabian` | the base is already published as `arabian`; the family follows it so it sorts together |
| `arabic_10k.json` | `arabian_10k` | the base is already published as `arabian`; the family follows it so it sorts together |
| `arabic_egypt.json` | `arabian_egypt` | the base is already published as `arabian`; the family follows it so it sorts together |
| `arabic_egypt_1k.json` | `arabian_egypt_1k` | the base is already published as `arabian`; the family follows it so it sorts together |
| `arabic_morocco.json` | `arabian_morocco` | the base is already published as `arabian`; the family follows it so it sorts together |
| `chinese_simplified.json` | `chinese` | the base is already published as `chinese`; the family follows it |
| `chinese_simplified_10k.json` | `chinese_10k` | the base is already published as `chinese`; the family follows it |
| `chinese_simplified_1k.json` | `chinese_1k` | the base is already published as `chinese`; the family follows it |
| `chinese_simplified_50k.json` | `chinese_50k` | the base is already published as `chinese`; the family follows it |
| `chinese_simplified_5k.json` | `chinese_5k` | the base is already published as `chinese`; the family follows it |
| `chinese_traditional.json` | `traditional_chinese` | the base is already published as `traditional_chinese`; the family follows it |
| `chinese_traditional_10k.json` | `traditional_chinese_10k` | the base is already published as `traditional_chinese`; the family follows it |
| `chinese_traditional_1k.json` | `traditional_chinese_1k` | the base is already published as `traditional_chinese`; the family follows it |
| `chinese_traditional_50k.json` | `traditional_chinese_50k` | the base is already published as `traditional_chinese`; the family follows it |
| `chinese_traditional_5k.json` | `traditional_chinese_5k` | the base is already published as `traditional_chinese`; the family follows it |
| `code_c++.json` | `code_cpp` | sanitised to `^[a-z0-9_]+$`; `++` is spelled `pp` |

A family is renamed **whole**, never just its base. Renaming only `arabic` →
`arabian` would leave `arabian` and `arabic_10k` in different halves of a
catalogue ordered by key, which is precisely the sorting property the
`code_<lang>` clause exists to buy.

## Normalisation

Bodies are vendored with LF endings (`.gitattributes`) and re-serialised with
2-space indentation, keeping four fields:

| Kept | Why |
|---|---|
| `name` | the core bundle's `dictName`; read by `generateWords` |
| `words` | the list; the only input to `dictVersion` |
| `bcp47` | the frontend sets it as the typing field's `lang` attribute |
| `rightToleft` | text direction for the 21 RTL corpora |

Dropped: `noLazyMode`, `orderedByFrequency`, `additionalAccents`,
`joiningScript`, `preferredFont`, `originalPunctuation`, `_comment`. A grep over
`internal/replay/corejs/core.bundle.js` returns **zero** hits for every one of
them, and none is read by the frontend either — they are monkeytype engine
metadata that would otherwise be served to every client forever.

**The RTL flag is re-spelled.** Upstream now writes `rightToLeft`; the ten
already-published bodies and `DictionaryBodySchema` on the frontend both write
`rightToleft`. Vendoring upstream's spelling verbatim would give the served
corpus two names for one flag and silently drop text direction on all 21
newly imported RTL languages — so the flag is normalised to the corpus's
existing spelling. No published body changes.

## Skips

Skipping is **structural only**. Nothing here is skipped for being exotic: RTL
and CJK ship, and their rendering quirks are catalogued below rather than used
as a reason.

### Key already published (8)

The immutability rule, applied at import time. A published `dict_hash` is frozen
forever, so a file whose canonical key already exists cannot overwrite it —
that would make every run recorded against the old hash unreplayable
(docs/DICTIONARIES.md). Two of them are byte-identical no-ops; the other six are
genuinely different word lists that upstream publishes under a name we already
use for something else.

- `arabic.json` → `arabian` — key arabian is already published with a different word list; overwriting would move a frozen dict_hash
- `chinese_simplified.json` → `chinese` — key chinese is already published with a different word list; overwriting would move a frozen dict_hash
- `chinese_traditional.json` → `traditional_chinese` — key traditional_chinese is already published with a different word list; overwriting would move a frozen dict_hash
- `code_css.json` → `code_css` — key code_css is already published with a different word list; overwriting would move a frozen dict_hash
- `english.json` → `english` — already published as english with identical content
- `french.json` → `french` — already published as french with identical content
- `german.json` → `german` — key german is already published with a different word list; overwriting would move a frozen dict_hash
- `russian.json` → `russian` — key russian is already published with a different word list; overwriting would move a frozen dict_hash

The remedy the doc prescribes is a **new** key, not an edit. None is taken here:
picking `german_upstream` or `german_v2` for six languages is a naming decision
with no way back, and this stage's brief is an import, not a re-publication.
Recorded as a deferred decision instead.

### Deferred by the size budget (9)

The budget is 60 MB of embedded dictionary bytes. Importing everything would
embed 140.96 MB, so import stopped by size descending — the largest files buy
the fewest languages per byte, and every one of them is a huge-N variant whose
base language ships anyway.

| File | Key | MB | Base language still shipped as |
|---|---|---|---|
| `norwegian_bokmal_600k.json` | `norwegian_bokmal_600k` | 12.32 | `norwegian_bokmal`, `norwegian_bokmal_10k`, `norwegian_bokmal_150k`, `norwegian_bokmal_1k`, `norwegian_bokmal_5k` |
| `french_600k.json` | `french_600k` | 12.23 | `french_10k`, `french_1k`, `french_2k`, `french_bitoduc` |
| `spanish_650k.json` | `spanish_650k` | 11.98 | `spanish`, `spanish_10k`, `spanish_1k` |
| `portuguese_550k.json` | `portuguese_550k` | 10.40 | `portuguese`, `portuguese_1k`, `portuguese_3k`, `portuguese_5k`, `portuguese_acentos_e_cedilha` |
| `russian_375k.json` | `russian_375k` | 10.07 | `russian_10k`, `russian_1k`, `russian_25k`, `russian_50k`, `russian_5k`, `russian_abbreviations`, `russian_contractions`, `russian_contractions_1k` |
| `english_450k.json` | `english_450k` | 7.91 | `english_10k`, `english_1k`, `english_25k`, `english_5k`, `english_commonly_misspelled`, `english_contractions`, `english_doubleletter`, `english_legal`, `english_medical`, `english_old`, `english_shakespearean` |
| `norwegian_nynorsk_400k.json` | `norwegian_nynorsk_400k` | 7.68 | `norwegian_nynorsk`, `norwegian_nynorsk_100k`, `norwegian_nynorsk_10k`, `norwegian_nynorsk_1k`, `norwegian_nynorsk_5k` |
| `portuguese_320k.json` | `portuguese_320k` | 6.00 | `portuguese`, `portuguese_1k`, `portuguese_3k`, `portuguese_5k`, `portuguese_acentos_e_cedilha` |
| `italian_280k.json` | `italian_280k` | 5.00 | `italian`, `italian_1k`, `italian_60k`, `italian_7k` |

No player loses a language to this: every deferred file is a larger word list
for a language that is imported at a smaller size.

## Rendering quirks, catalogued

Catalogued, not blocking — the arabian/chinese precedent. These are properties
of the text a client renders, and none of them is a reason to withhold a corpus:

- **21 right-to-left corpora** (`arabian_10k`, `arabian_egypt`, `arabian_egypt_1k`, `arabian_morocco`, `hebrew`, `hebrew_10k`, …) carry
  `rightToleft: true`. The flag is on the body, so the field can set direction
  from the same fetch that gives it the words.
- **CJK and Thai have no spaces between words.** `chinese*`, `traditional_chinese*`,
  `japanese_*`, `korean`, `thai*`, `lao`, `khmer`, `myanmar_burmese` and
  `tibetan` all generate space-joined tokens the way every other language does;
  the text is legible but not idiomatic. It is what upstream serves and what the
  quote corpora already assume — `chinese`'s per-corpus 30/80/200 length
  thresholds exist for exactly this reason (docs/QUOTES.md).
- **Combining marks and indic conjuncts** (`hindi`, `bangla`, `tamil`,
  `telugu`, `kannada`, `malayalam`, `sinhala`, `santali`, …) mean one grapheme
  is several code points. The core counts events per grapheme, and
  `TestEveryPublishedDictionaryCanPlayAFullLengthRun` is what proves each of
  these still fits under the ingestion caps.

## The inventory

| Upstream file | Key | Display name | Bytes | Words | Status |
|---|---|---|---|---|---|
| `afrikaans.json` | `afrikaans` | Afrikaans | 2 302 | 180 | import |
| `afrikaans_10k.json` | `afrikaans_10k` | Afrikaans (10k) | 127 053 | 8 164 | import |
| `afrikaans_1k.json` | `afrikaans_1k` | Afrikaans (1k) | 15 603 | 1 000 | import |
| `albanian.json` | `albanian` | Albanian | 2 426 | 195 | import |
| `albanian_1k.json` | `albanian_1k` | Albanian (1k) | 12 951 | 896 | import |
| `amharic.json` | `amharic` | Amharic | 5 254 | 273 | import |
| `amharic_1k.json` | `amharic_1k` | Amharic (1k) | 21 395 | 1 001 | import |
| `amharic_5k.json` | `amharic_5k` | Amharic (5k) | 110 721 | 5 000 | import |
| `arabic.json` | `arabian` | — | 5 093 | 199 | skip — key arabian is already published with a different word list; overwriting would move a frozen dict_hash |
| `arabic_10k.json` | `arabian_10k` | Arabic (10k) | 259 105 | 9 281 | import |
| `arabic_egypt.json` | `arabian_egypt` | Arabic (Egyptian) | 3 364 | 210 | import |
| `arabic_egypt_1k.json` | `arabian_egypt_1k` | Arabic (Egyptian, 1k) | 19 609 | 1 141 | import |
| `arabic_morocco.json` | `arabian_morocco` | Arabic (Moroccan) | 3 788 | 229 | import |
| `armenian.json` | `armenian` | Armenian | 3 966 | 200 | import |
| `armenian_1k.json` | `armenian_1k` | Armenian (1k) | 20 515 | 1 000 | import |
| `armenian_western.json` | `armenian_western` | Armenian (Western) | 3 828 | 200 | import |
| `armenian_western_1k.json` | `armenian_western_1k` | Armenian (Western, 1k) | 21 615 | 1 000 | import |
| `azerbaijani.json` | `azerbaijani` | Azerbaijani | 2 812 | 199 | import |
| `azerbaijani_1k.json` | `azerbaijani_1k` | Azerbaijani (1k) | 13 856 | 989 | import |
| `bangla.json` | `bangla` | Bangla | 3 917 | 199 | import |
| `bangla_10k.json` | `bangla_10k` | Bangla (10k) | 245 972 | 9 734 | import |
| `bangla_letters.json` | `bangla_letters` | Bangla (letters) | 780 | 62 | import |
| `bashkir.json` | `bashkir` | Bashkir | 3 486 | 207 | import |
| `belarusian.json` | `belarusian` | Belarusian | 3 112 | 200 | import |
| `belarusian_100k.json` | `belarusian_100k` | Belarusian (100k) | 2 904 588 | 106 381 | import |
| `belarusian_10k.json` | `belarusian_10k` | Belarusian (10k) | 213 829 | 10 725 | import |
| `belarusian_1k.json` | `belarusian_1k` | Belarusian (1k) | 19 687 | 997 | import |
| `belarusian_25k.json` | `belarusian_25k` | Belarusian (25k) | 480 840 | 24 133 | import |
| `belarusian_50k.json` | `belarusian_50k` | Belarusian (50k) | 1 199 619 | 52 817 | import |
| `belarusian_5k.json` | `belarusian_5k` | Belarusian (5k) | 100 666 | 5 044 | import |
| `belarusian_lacinka.json` | `belarusian_lacinka` | Belarusian (Łacinka) | 2 488 | 200 | import |
| `belarusian_lacinka_1k.json` | `belarusian_lacinka_1k` | Belarusian (Łacinka, 1k) | 14 587 | 997 | import |
| `bemba.json` | `bemba` | Bemba | 2 869 | 201 | import |
| `bemba_10k.json` | `bemba_10k` | Bemba (10k) | 158 205 | 10 001 | import |
| `bemba_1k.json` | `bemba_1k` | Bemba (1k) | 14 807 | 1 001 | import |
| `bosnian.json` | `bosnian` | Bosnian | 2 751 | 189 | import |
| `bosnian_4k.json` | `bosnian_4k` | Bosnian (4k) | 58 014 | 3 816 | import |
| `bulgarian.json` | `bulgarian` | Bulgarian | 3 574 | 214 | import |
| `bulgarian_1k.json` | `bulgarian_1k` | Bulgarian (1k) | 22 769 | 1 172 | import |
| `bulgarian_latin.json` | `bulgarian_latin` | Bulgarian (Latin) | 2 707 | 213 | import |
| `bulgarian_latin_1k.json` | `bulgarian_latin_1k` | Bulgarian (Latin, 1k) | 16 436 | 1 169 | import |
| `catalan.json` | `catalan` | Catalan | 2 600 | 200 | import |
| `catalan_1k.json` | `catalan_1k` | Catalan (1k) | 14 307 | 1 000 | import |
| `chinese_simplified.json` | `chinese` | — | 2 875 | 200 | skip — key chinese is already published with a different word list; overwriting would move a frozen dict_hash |
| `chinese_simplified_10k.json` | `chinese_10k` | Chinese (simplified, 10k) | 143 799 | 10 000 | import |
| `chinese_simplified_1k.json` | `chinese_1k` | Chinese (simplified, 1k) | 14 129 | 1 000 | import |
| `chinese_simplified_50k.json` | `chinese_50k` | Chinese (simplified, 50k) | 753 482 | 50 000 | import |
| `chinese_simplified_5k.json` | `chinese_5k` | Chinese (simplified, 5k) | 71 461 | 5 000 | import |
| `chinese_traditional.json` | `traditional_chinese` | — | 2 701 | 200 | skip — key traditional_chinese is already published with a different word list; overwriting would move a frozen dict_hash |
| `chinese_traditional_10k.json` | `traditional_chinese_10k` | Chinese (traditional, 10k) | 143 456 | 9 974 | import |
| `chinese_traditional_1k.json` | `traditional_chinese_1k` | Chinese (traditional, 1k) | 14 132 | 1 000 | import |
| `chinese_traditional_50k.json` | `traditional_chinese_50k` | Chinese (traditional, 50k) | 752 498 | 49 925 | import |
| `chinese_traditional_5k.json` | `traditional_chinese_5k` | Chinese (traditional, 5k) | 71 362 | 4 991 | import |
| `code_6502_assembly.json` | `code_6502_assembly` | 6502 assembly (code) | 668 | 56 | import |
| `code_abap.json` | `code_abap` | ABAP (code) | 2 928 | 200 | import |
| `code_abap_1k.json` | `code_abap_1k` | ABAP (code, 1k) | 17 432 | 1 111 | import |
| `code_arduino.json` | `code_arduino` | Arduino (code) | 1 607 | 104 | import |
| `code_assembly.json` | `code_assembly` | Assembly (code) | 978 | 81 | import |
| `code_bash.json` | `code_bash` | Bash (code) | 3 587 | 276 | import |
| `code_brainfck.json` | `code_brainfck` | Brainfuck (code) | 4 053 | 194 | import |
| `code_c++.json` | `code_cpp` | C++ (code) | 1 661 | 111 | import |
| `code_c.json` | `code_c` | C (code) | 2 975 | 200 | import |
| `code_clojure.json` | `code_clojure` | Clojure (code) | 3 032 | 212 | import |
| `code_cobol.json` | `code_cobol` | COBOL (code) | 1 681 | 105 | import |
| `code_common_lisp.json` | `code_common_lisp` | Common Lisp (code) | 19 148 | 978 | import |
| `code_csharp.json` | `code_csharp` | C# (code) | 1 789 | 130 | import |
| `code_css.json` | `code_css` | — | 1 263 | 72 | skip — key code_css is already published with a different word list; overwriting would move a frozen dict_hash |
| `code_cuda.json` | `code_cuda` | CUDA (code) | 6 816 | 237 | import |
| `code_dart.json` | `code_dart` | Dart (code) | 1 007 | 73 | import |
| `code_elixir.json` | `code_elixir` | Elixir (code) | 10 103 | 554 | import |
| `code_erlang.json` | `code_erlang` | Erlang (code) | 3 764 | 245 | import |
| `code_fortran.json` | `code_fortran` | Fortran (code) | 2 897 | 200 | import |
| `code_fsharp.json` | `code_fsharp` | F# (code) | 1 410 | 102 | import |
| `code_gdscript.json` | `code_gdscript` | GDScript (code) | 1 568 | 103 | import |
| `code_gdscript_2.json` | `code_gdscript_2` | GDScript 2 (code) | 1 561 | 102 | import |
| `code_gleam.json` | `code_gleam` | Gleam (code) | 9 099 | 442 | import |
| `code_go.json` | `code_go` | Go (code) | 863 | 63 | import |
| `code_haskell.json` | `code_haskell` | Haskell (code) | 2 708 | 208 | import |
| `code_html.json` | `code_html` | HTML (code) | 3 386 | 232 | import |
| `code_java.json` | `code_java` | Java (code) | 1 261 | 89 | import |
| `code_javascript.json` | `code_javascript` | JavaScript (code) | 2 017 | 126 | import |
| `code_javascript_1k.json` | `code_javascript_1k` | JavaScript (code, 1k) | 14 398 | 1 001 | import |
| `code_javascript_react.json` | `code_javascript_react` | JavaScript React (code) | 3 675 | 202 | import |
| `code_jule.json` | `code_jule` | Jule (code) | 762 | 59 | import |
| `code_julia.json` | `code_julia` | Julia (code) | 1 465 | 103 | import |
| `code_kotlin.json` | `code_kotlin` | Kotlin (code) | 1 165 | 85 | import |
| `code_latex.json` | `code_latex` | LaTeX (code) | 4 200 | 200 | import |
| `code_lua.json` | `code_lua` | Lua (code) | 845 | 59 | import |
| `code_luau.json` | `code_luau` | Luau (code) | 1 060 | 73 | import |
| `code_matlab.json` | `code_matlab` | MATLAB (code) | 882 | 63 | import |
| `code_nim.json` | `code_nim` | Nim (code) | 1 190 | 99 | import |
| `code_nix.json` | `code_nix` | Nix (code) | 2 110 | 125 | import |
| `code_ocaml.json` | `code_ocaml` | OCaml (code) | 7 869 | 495 | import |
| `code_odin.json` | `code_odin` | Odin (code) | 1 820 | 129 | import |
| `code_ook.json` | `code_ook` | Ook! (code) | 195 | 9 | import |
| `code_opencl.json` | `code_opencl` | OpenCL (code) | 3 104 | 221 | import |
| `code_pascal.json` | `code_pascal` | Pascal (code) | 2 006 | 151 | import |
| `code_perl.json` | `code_perl` | Perl (code) | 3 258 | 234 | import |
| `code_php.json` | `code_php` | PHP (code) | 3 933 | 296 | import |
| `code_powershell.json` | `code_powershell` | PowerShell (code) | 2 255 | 106 | import |
| `code_python.json` | `code_python` | Python (code) | 2 414 | 174 | import |
| `code_python_1k.json` | `code_python_1k` | Python (code, 1k) | 18 039 | 1 096 | import |
| `code_python_2k.json` | `code_python_2k` | Python (code, 2k) | 35 927 | 2 060 | import |
| `code_python_5k.json` | `code_python_5k` | Python (code, 5k) | 96 213 | 5 235 | import |
| `code_r.json` | `code_r` | R (code) | 320 | 21 | import |
| `code_r_2k.json` | `code_r_2k` | R (code, 2k) | 40 955 | 2 270 | import |
| `code_rockstar.json` | `code_rockstar` | Rockstar (code) | 1 299 | 103 | import |
| `code_ruby.json` | `code_ruby` | Ruby (code) | 1 970 | 118 | import |
| `code_rust.json` | `code_rust` | Rust (code) | 3 014 | 192 | import |
| `code_scala.json` | `code_scala` | Scala (code) | 1 279 | 96 | import |
| `code_sql.json` | `code_sql` | SQL (code) | 2 532 | 174 | import |
| `code_swift.json` | `code_swift` | Swift (code) | 1 002 | 69 | import |
| `code_systemverilog.json` | `code_systemverilog` | SystemVerilog (code) | 3 273 | 222 | import |
| `code_typescript.json` | `code_typescript` | TypeScript (code) | 2 947 | 198 | import |
| `code_typst.json` | `code_typst` | Typst (code) | 571 | 43 | import |
| `code_v.json` | `code_v` | V (code) | 847 | 64 | import |
| `code_vhdl.json` | `code_vhdl` | VHDL (code) | 2 069 | 145 | import |
| `code_vim.json` | `code_vim` | Vim (code) | 1 866 | 167 | import |
| `code_vimscript.json` | `code_vimscript` | Vimscript (code) | 1 238 | 85 | import |
| `code_visual_basic.json` | `code_visual_basic` | Visual Basic (code) | 2 542 | 180 | import |
| `code_yoptascript.json` | `code_yoptascript` | YoptaScript (code) | 6 103 | 239 | import |
| `code_zig.json` | `code_zig` | Zig (code) | 2 294 | 146 | import |
| `croatian.json` | `croatian` | Croatian | 2 848 | 212 | import |
| `croatian_1k.json` | `croatian_1k` | Croatian (1k) | 15 561 | 1 108 | import |
| `czech.json` | `czech` | Czech | 2 629 | 199 | import |
| `czech_10k.json` | `czech_10k` | Czech (10k) | 153 767 | 9 629 | import |
| `czech_1k.json` | `czech_1k` | Czech (1k) | 12 435 | 899 | import |
| `danish.json` | `danish` | Danish | 2 446 | 196 | import |
| `danish_10k.json` | `danish_10k` | Danish (10k) | 152 883 | 9 624 | import |
| `danish_1k.json` | `danish_1k` | Danish (1k) | 13 335 | 954 | import |
| `docker_file.json` | `docker_file` | Dockerfile (code) | 303 | 19 | import |
| `dutch.json` | `dutch` | Dutch | 2 611 | 199 | import |
| `dutch_10k.json` | `dutch_10k` | Dutch (10k) | 195 728 | 9 998 | import |
| `dutch_1k.json` | `dutch_1k` | Dutch (1k) | 13 661 | 1 000 | import |
| `english.json` | `english` | — | 2 488 | 200 | skip — already published as english with identical content |
| `english_10k.json` | `english_10k` | English (10k) | 151 256 | 9 944 | import |
| `english_1k.json` | `english_1k` | English (1k) | 12 891 | 1 000 | import |
| `english_25k.json` | `english_25k` | English (25k) | 381 642 | 24 141 | import |
| `english_450k.json` | `english_450k` | — | 7 911 746 | 450 029 | skip — deferred by the 60 MB embed budget (7.91 MB, largest remaining) |
| `english_5k.json` | `english_5k` | English (5k) | 73 917 | 5 000 | import |
| `english_commonly_misspelled.json` | `english_commonly_misspelled` | English (commonly misspelled) | 28 456 | 1 729 | import |
| `english_contractions.json` | `english_contractions` | English (contractions) | 2 660 | 183 | import |
| `english_doubleletter.json` | `english_doubleletter` | English (double letters) | 2 977 | 202 | import |
| `english_legal.json` | `english_legal` | English (legal) | 44 709 | 1 102 | import |
| `english_medical.json` | `english_medical` | English (medical) | 10 232 | 580 | import |
| `english_old.json` | `english_old` | Old English | 2 688 | 200 | import |
| `english_shakespearean.json` | `english_shakespearean` | English (Shakespearean) | 2 763 | 193 | import |
| `esperanto.json` | `esperanto` | Esperanto | 2 455 | 200 | import |
| `esperanto_10k.json` | `esperanto_10k` | Esperanto (10k) | 148 893 | 10 000 | import |
| `esperanto_1k.json` | `esperanto_1k` | Esperanto (1k) | 13 524 | 1 000 | import |
| `esperanto_25k.json` | `esperanto_25k` | Esperanto (25k) | 392 244 | 24 998 | import |
| `esperanto_36k.json` | `esperanto_36k` | Esperanto (36k) | 578 620 | 36 342 | import |
| `esperanto_h_sistemo.json` | `esperanto_h_sistemo` | Esperanto (h-sistemo) | 2 457 | 200 | import |
| `esperanto_h_sistemo_10k.json` | `esperanto_h_sistemo_10k` | Esperanto (h-sistemo, 10k) | 148 310 | 9 969 | import |
| `esperanto_h_sistemo_1k.json` | `esperanto_h_sistemo_1k` | Esperanto (h-sistemo, 1k) | 13 485 | 999 | import |
| `esperanto_h_sistemo_25k.json` | `esperanto_h_sistemo_25k` | Esperanto (h-sistemo, 25k) | 390 651 | 24 922 | import |
| `esperanto_h_sistemo_36k.json` | `esperanto_h_sistemo_36k` | Esperanto (h-sistemo, 36k) | 574 672 | 36 131 | import |
| `esperanto_x_sistemo.json` | `esperanto_x_sistemo` | Esperanto (x-sistemo) | 2 465 | 200 | import |
| `esperanto_x_sistemo_10k.json` | `esperanto_x_sistemo_10k` | Esperanto (x-sistemo, 10k) | 148 820 | 9 993 | import |
| `esperanto_x_sistemo_1k.json` | `esperanto_x_sistemo_1k` | Esperanto (x-sistemo, 1k) | 13 523 | 999 | import |
| `esperanto_x_sistemo_25k.json` | `esperanto_x_sistemo_25k` | Esperanto (x-sistemo, 25k) | 391 861 | 24 970 | import |
| `esperanto_x_sistemo_36k.json` | `esperanto_x_sistemo_36k` | Esperanto (x-sistemo, 36k) | 577 965 | 36 296 | import |
| `estonian.json` | `estonian` | Estonian | 2 564 | 200 | import |
| `estonian_10k.json` | `estonian_10k` | Estonian (10k) | 155 889 | 10 000 | import |
| `estonian_1k.json` | `estonian_1k` | Estonian (1k) | 13 905 | 1 000 | import |
| `estonian_5k.json` | `estonian_5k` | Estonian (5k) | 75 330 | 5 000 | import |
| `euskera.json` | `euskera` | Basque | 2 710 | 199 | import |
| `filipino.json` | `filipino` | Filipino | 2 587 | 200 | import |
| `filipino_1k.json` | `filipino_1k` | Filipino (1k) | 14 109 | 1 000 | import |
| `finnish.json` | `finnish` | Finnish | 2 958 | 204 | import |
| `finnish_10k.json` | `finnish_10k` | Finnish (10k) | 180 355 | 9 906 | import |
| `finnish_1k.json` | `finnish_1k` | Finnish (1k) | 14 789 | 1 000 | import |
| `french.json` | `french` | — | 2 341 | 174 | skip — already published as french with identical content |
| `french_10k.json` | `french_10k` | French (10k) | 163 289 | 10 251 | import |
| `french_1k.json` | `french_1k` | French (1k) | 20 100 | 1 394 | import |
| `french_2k.json` | `french_2k` | French (2k) | 30 991 | 2 041 | import |
| `french_600k.json` | `french_600k` | — | 12 225 530 | 633 941 | skip — deferred by the 60 MB embed budget (12.23 MB, largest remaining) |
| `french_bitoduc.json` | `french_bitoduc` | French (Bitoduc) | 2 558 | 138 | import |
| `frisian.json` | `frisian` | Frisian | 2 653 | 200 | import |
| `frisian_1k.json` | `frisian_1k` | Frisian (1k) | 12 416 | 909 | import |
| `friulian.json` | `friulian` | Friulian | 2 712 | 200 | import |
| `galician.json` | `galician` | Galician | 2 517 | 200 | import |
| `georgian.json` | `georgian` | Georgian | 5 713 | 200 | import |
| `german.json` | `german` | — | 2 586 | 200 | skip — key german is already published with a different word list; overwriting would move a frozen dict_hash |
| `german_10k.json` | `german_10k` | German (10k) | 162 800 | 9 994 | import |
| `german_1k.json` | `german_1k` | German (1k) | 13 918 | 988 | import |
| `german_250k.json` | `german_250k` | German (250k) | 4 710 362 | 239 243 | import |
| `git.json` | `git` | Git (code) | 742 | 52 | import |
| `greek.json` | `greek` | Greek | 4 263 | 209 | import |
| `greek_10k.json` | `greek_10k` | Greek (10k) | 251 963 | 9 936 | import |
| `greek_1k.json` | `greek_1k` | Greek (1k) | 21 850 | 993 | import |
| `greek_25k.json` | `greek_25k` | Greek (25k) | 653 439 | 24 836 | import |
| `greek_5k.json` | `greek_5k` | Greek (5k) | 121 536 | 4 963 | import |
| `greek_koine.json` | `greek_koine` | Greek (Koine) | 3 820 | 199 | import |
| `greeklish.json` | `greeklish` | Greeklish | 3 006 | 209 | import |
| `greeklish_10k.json` | `greeklish_10k` | Greeklish (10k) | 165 357 | 9 808 | import |
| `greeklish_1k.json` | `greeklish_1k` | Greeklish (1k) | 14 995 | 990 | import |
| `greeklish_25k.json` | `greeklish_25k` | Greeklish (25k) | 421 371 | 24 273 | import |
| `greeklish_5k.json` | `greeklish_5k` | Greeklish (5k) | 80 826 | 4 926 | import |
| `gujarati.json` | `gujarati` | Gujarati | 5 110 | 214 | import |
| `gujarati_1k.json` | `gujarati_1k` | Gujarati (1k) | 21 775 | 1 004 | import |
| `hausa.json` | `hausa` | Hausa | 2 656 | 200 | import |
| `hausa_1k.json` | `hausa_1k` | Hausa (1k) | 11 528 | 829 | import |
| `hawaiian.json` | `hawaiian` | Hawaiian | 2 523 | 200 | import |
| `hawaiian_1k.json` | `hawaiian_1k` | Hawaiian (1k) | 13 689 | 1 000 | import |
| `hebrew.json` | `hebrew` | Hebrew | 3 331 | 198 | import |
| `hebrew_10k.json` | `hebrew_10k` | Hebrew (10k) | 181 715 | 10 000 | import |
| `hebrew_1k.json` | `hebrew_1k` | Hebrew (1k) | 16 632 | 1 000 | import |
| `hebrew_5k.json` | `hebrew_5k` | Hebrew (5k) | 88 540 | 5 000 | import |
| `hindi.json` | `hindi` | Hindi | 4 065 | 200 | import |
| `hindi_1k.json` | `hindi_1k` | Hindi (1k) | 22 145 | 999 | import |
| `hinglish.json` | `hinglish` | Hinglish | 2 667 | 212 | import |
| `hungarian.json` | `hungarian` | Hungarian | 2 675 | 200 | import |
| `hungarian_1k.json` | `hungarian_1k` | Hungarian (1k) | 14 826 | 1 000 | import |
| `hungarian_2k.json` | `hungarian_2k` | Hungarian (2k) | 38 607 | 2 452 | import |
| `icelandic.json` | `icelandic` | Icelandic | 2 745 | 214 | import |
| `icelandic_1k.json` | `icelandic_1k` | Icelandic (1k) | 13 627 | 1 000 | import |
| `indonesian.json` | `indonesian` | Indonesian | 4 209 | 310 | import |
| `indonesian_10k.json` | `indonesian_10k` | Indonesian (10k) | 230 579 | 13 769 | import |
| `indonesian_1k.json` | `indonesian_1k` | Indonesian (1k) | 14 949 | 1 020 | import |
| `irish.json` | `irish` | Irish | 2 387 | 183 | import |
| `irish_1k.json` | `irish_1k` | Irish (1k) | 14 191 | 1 000 | import |
| `italian.json` | `italian` | Italian | 2 878 | 199 | import |
| `italian_1k.json` | `italian_1k` | Italian (1k) | 17 212 | 1 159 | import |
| `italian_280k.json` | `italian_280k` | — | 5 004 897 | 279 833 | skip — deferred by the 60 MB embed budget (5.00 MB, largest remaining) |
| `italian_60k.json` | `italian_60k` | Italian (60k) | 984 995 | 60 442 | import |
| `italian_7k.json` | `italian_7k` | Italian (7k) | 114 063 | 7 154 | import |
| `japanese_hiragana.json` | `japanese_hiragana` | Japanese (hiragana) | 9 297 | 554 | import |
| `japanese_katakana.json` | `japanese_katakana` | Japanese (katakana) | 6 616 | 343 | import |
| `japanese_romaji.json` | `japanese_romaji` | Japanese (rōmaji) | 2 010 | 153 | import |
| `japanese_romaji_1k.json` | `japanese_romaji_1k` | Japanese (rōmaji, 1k) | 13 771 | 987 | import |
| `jyutping.json` | `jyutping` | Jyutping | 2 313 | 200 | import |
| `kabyle.json` | `kabyle` | Kabyle | 2 826 | 200 | import |
| `kabyle_10k.json` | `kabyle_10k` | Kabyle (10k) | 152 574 | 10 000 | import |
| `kabyle_1k.json` | `kabyle_1k` | Kabyle (1k) | 15 228 | 1 000 | import |
| `kabyle_2k.json` | `kabyle_2k` | Kabyle (2k) | 30 575 | 2 000 | import |
| `kabyle_5k.json` | `kabyle_5k` | Kabyle (5k) | 76 670 | 5 000 | import |
| `kannada.json` | `kannada` | Kannada | 4 688 | 200 | import |
| `kazakh.json` | `kazakh` | Kazakh | 3 232 | 202 | import |
| `kazakh_1k.json` | `kazakh_1k` | Kazakh (1k) | 19 022 | 990 | import |
| `khmer.json` | `khmer` | Khmer | 8 035 | 331 | import |
| `kinyarwanda.json` | `kinyarwanda` | Kinyarwanda | 5 499 | 368 | import |
| `klingon.json` | `klingon` | Klingon | 2 817 | 201 | import |
| `klingon_1k.json` | `klingon_1k` | Klingon (1k) | 13 499 | 1 001 | import |
| `kokanu.json` | `kokanu` | kokanu | 4 807 | 381 | import |
| `korean.json` | `korean` | Korean | 6 763 | 470 | import |
| `korean_1k.json` | `korean_1k` | Korean (1k) | 14 868 | 975 | import |
| `korean_5k.json` | `korean_5k` | Korean (5k) | 67 607 | 4 201 | import |
| `kurdish_central.json` | `kurdish_central` | Kurdish (Central) | 3 476 | 204 | import |
| `kurdish_central_2k.json` | `kurdish_central_2k` | Kurdish (Central, 2k) | 27 543 | 1 486 | import |
| `kurdish_central_4k.json` | `kurdish_central_4k` | Kurdish (Central, 4k) | 79 542 | 4 256 | import |
| `kyrgyz.json` | `kyrgyz` | Kyrgyz | 4 433 | 234 | import |
| `kyrgyz_1k.json` | `kyrgyz_1k` | Kyrgyz (1k) | 16 228 | 849 | import |
| `lao.json` | `lao` | Lao | 13 374 | 537 | import |
| `latin.json` | `latin` | Latin | 4 676 | 362 | import |
| `latvian.json` | `latvian` | Latvian | 2 472 | 179 | import |
| `latvian_1k.json` | `latvian_1k` | Latvian (1k) | 13 834 | 930 | import |
| `league_of_legends.json` | `league_of_legends` | League of Legends | 8 320 | 442 | import |
| `likanu.json` | `likanu` | likanu | 5 889 | 381 | import |
| `lithuanian.json` | `lithuanian` | Lithuanian | 2 789 | 199 | import |
| `lithuanian_1k.json` | `lithuanian_1k` | Lithuanian (1k) | 15 084 | 990 | import |
| `lithuanian_3k.json` | `lithuanian_3k` | Lithuanian (3k) | 47 828 | 2 978 | import |
| `lojban_cmavo.json` | `lojban_cmavo` | Lojban (cmavo) | 7 858 | 674 | import |
| `lojban_gismu.json` | `lojban_gismu` | Lojban (gismu) | 18 142 | 1 392 | import |
| `lorem_ipsum.json` | `lorem_ipsum` | Lorem ipsum | 2 694 | 185 | import |
| `macedonian.json` | `macedonian` | Macedonian | 3 062 | 173 | import |
| `macedonian_10k.json` | `macedonian_10k` | Macedonian (10k) | 215 264 | 10 000 | import |
| `macedonian_1k.json` | `macedonian_1k` | Macedonian (1k) | 17 981 | 901 | import |
| `macedonian_75k.json` | `macedonian_75k` | Macedonian (75k) | 1 822 830 | 75 000 | import |
| `malagasy.json` | `malagasy` | Malagasy | 2 422 | 197 | import |
| `malagasy_1k.json` | `malagasy_1k` | Malagasy (1k) | 16 574 | 975 | import |
| `malay.json` | `malay` | Malay | 3 095 | 228 | import |
| `malay_1k.json` | `malay_1k` | Malay (1k) | 14 561 | 1 000 | import |
| `malayalam.json` | `malayalam` | Malayalam | 4 495 | 200 | import |
| `maltese.json` | `maltese` | Maltese | 2 438 | 183 | import |
| `maltese_1k.json` | `maltese_1k` | Maltese (1k) | 13 191 | 927 | import |
| `maori_1k.json` | `maori_1k` | Māori (1k) | 13 441 | 975 | import |
| `marathi.json` | `marathi` | Marathi | 4 342 | 201 | import |
| `mongolian.json` | `mongolian` | Mongolian | 23 021 | 1 085 | import |
| `mongolian_10k.json` | `mongolian_10k` | Mongolian (10k) | 180 041 | 9 219 | import |
| `myanmar_burmese.json` | `myanmar_burmese` | Burmese | 6 250 | 200 | import |
| `nepali.json` | `nepali` | Nepali | 4 681 | 200 | import |
| `nepali_1k.json` | `nepali_1k` | Nepali (1k) | 22 214 | 1 000 | import |
| `nepali_romanized.json` | `nepali_romanized` | Nepali (romanised) | 6 118 | 430 | import |
| `norwegian_bokmal.json` | `norwegian_bokmal` | Norwegian (Bokmål) | 2 475 | 200 | import |
| `norwegian_bokmal_10k.json` | `norwegian_bokmal_10k` | Norwegian (Bokmål, 10k) | 159 238 | 10 000 | import |
| `norwegian_bokmal_150k.json` | `norwegian_bokmal_150k` | Norwegian (Bokmål, 150k) | 2 677 450 | 142 938 | import |
| `norwegian_bokmal_1k.json` | `norwegian_bokmal_1k` | Norwegian (Bokmål, 1k) | 13 964 | 1 000 | import |
| `norwegian_bokmal_5k.json` | `norwegian_bokmal_5k` | Norwegian (Bokmål, 5k) | 76 654 | 5 000 | import |
| `norwegian_bokmal_600k.json` | `norwegian_bokmal_600k` | — | 12 320 924 | 614 970 | skip — deferred by the 60 MB embed budget (12.32 MB, largest remaining) |
| `norwegian_nynorsk.json` | `norwegian_nynorsk` | Norwegian (Nynorsk) | 3 188 | 250 | import |
| `norwegian_nynorsk_100k.json` | `norwegian_nynorsk_100k` | Norwegian (Nynorsk, 100k) | 1 840 291 | 104 745 | import |
| `norwegian_nynorsk_10k.json` | `norwegian_nynorsk_10k` | Norwegian (Nynorsk, 10k) | 162 411 | 9 939 | import |
| `norwegian_nynorsk_1k.json` | `norwegian_nynorsk_1k` | Norwegian (Nynorsk, 1k) | 13 954 | 1 000 | import |
| `norwegian_nynorsk_400k.json` | `norwegian_nynorsk_400k` | — | 7 680 983 | 410 719 | skip — deferred by the 60 MB embed budget (7.68 MB, largest remaining) |
| `norwegian_nynorsk_5k.json` | `norwegian_nynorsk_5k` | Norwegian (Nynorsk, 5k) | 78 042 | 5 000 | import |
| `occitan.json` | `occitan` | Occitan | 2 470 | 200 | import |
| `occitan_10k.json` | `occitan_10k` | Occitan (10k) | 148 964 | 10 000 | import |
| `occitan_1k.json` | `occitan_1k` | Occitan (1k) | 13 459 | 1 000 | import |
| `occitan_2k.json` | `occitan_2k` | Occitan (2k) | 28 013 | 2 000 | import |
| `occitan_5k.json` | `occitan_5k` | Occitan (5k) | 72 816 | 5 000 | import |
| `oromo.json` | `oromo` | Oromo | 2 709 | 200 | import |
| `oromo_1k.json` | `oromo_1k` | Oromo (1k) | 14 980 | 1 000 | import |
| `oromo_5k.json` | `oromo_5k` | Oromo (5k) | 83 072 | 5 000 | import |
| `pashto.json` | `pashto` | Pashto | 3 338 | 192 | import |
| `persian.json` | `persian` | Persian | 3 387 | 196 | import |
| `persian_1k.json` | `persian_1k` | Persian (1k) | 17 008 | 1 000 | import |
| `persian_20k.json` | `persian_20k` | Persian (20k) | 391 038 | 21 715 | import |
| `persian_5k.json` | `persian_5k` | Persian (5k) | 89 304 | 5 000 | import |
| `persian_romanized.json` | `persian_romanized` | Persian (romanised) | 2 948 | 195 | import |
| `pig_latin.json` | `pig_latin` | Pig Latin | 2 850 | 197 | import |
| `pinyin.json` | `pinyin` | Pinyin | 2 152 | 175 | import |
| `pinyin_10k.json` | `pinyin_10k` | Pinyin (10k) | 15 754 | 1 293 | import |
| `pinyin_1k.json` | `pinyin_1k` | Pinyin (1k) | 7 471 | 612 | import |
| `pokemon_1k.json` | `pokemon_1k` | Pokémon (1k) | 16 132 | 1 025 | import |
| `polish.json` | `polish` | Polish | 2 558 | 183 | import |
| `polish_10k.json` | `polish_10k` | Polish (10k) | 160 756 | 10 000 | import |
| `polish_200k.json` | `polish_200k` | Polish (200k) | 3 622 698 | 199 979 | import |
| `polish_20k.json` | `polish_20k` | Polish (20k) | 330 623 | 20 000 | import |
| `polish_2k.json` | `polish_2k` | Polish (2k) | 35 311 | 2 338 | import |
| `polish_40k.json` | `polish_40k` | Polish (40k) | 690 518 | 40 000 | import |
| `polish_5k.json` | `polish_5k` | Polish (5k) | 78 342 | 5 000 | import |
| `portuguese.json` | `portuguese` | Portuguese | 2 747 | 201 | import |
| `portuguese_1k.json` | `portuguese_1k` | Portuguese (1k) | 14 738 | 1 000 | import |
| `portuguese_320k.json` | `portuguese_320k` | — | 5 998 624 | 318 601 | skip — deferred by the 60 MB embed budget (6.00 MB, largest remaining) |
| `portuguese_3k.json` | `portuguese_3k` | Portuguese (3k) | 49 279 | 3 043 | import |
| `portuguese_550k.json` | `portuguese_550k` | — | 10 398 963 | 558 207 | skip — deferred by the 60 MB embed budget (10.40 MB, largest remaining) |
| `portuguese_5k.json` | `portuguese_5k` | Portuguese (5k) | 92 691 | 5 665 | import |
| `portuguese_acentos_e_cedilha.json` | `portuguese_acentos_e_cedilha` | Portuguese (accents & cedilla) | 36 183 | 1 999 | import |
| `quenya.json` | `quenya` | Quenya | 26 538 | 1 883 | import |
| `romanian.json` | `romanian` | Romanian | 3 570 | 270 | import |
| `romanian_100k.json` | `romanian_100k` | Romanian (100k) | 1 687 365 | 100 000 | import |
| `romanian_10k.json` | `romanian_10k` | Romanian (10k) | 168 800 | 10 000 | import |
| `romanian_1k.json` | `romanian_1k` | Romanian (1k) | 16 690 | 1 000 | import |
| `romanian_200k.json` | `romanian_200k` | Romanian (200k) | 3 374 931 | 200 000 | import |
| `romanian_25k.json` | `romanian_25k` | Romanian (25k) | 422 202 | 25 000 | import |
| `romanian_50k.json` | `romanian_50k` | Romanian (50k) | 842 909 | 50 000 | import |
| `romanian_5k.json` | `romanian_5k` | Romanian (5k) | 83 912 | 5 000 | import |
| `russian.json` | `russian` | — | 3 481 | 200 | skip — key russian is already published with a different word list; overwriting would move a frozen dict_hash |
| `russian_10k.json` | `russian_10k` | Russian (10k) | 238 859 | 9 996 | import |
| `russian_1k.json` | `russian_1k` | Russian (1k) | 20 221 | 996 | import |
| `russian_25k.json` | `russian_25k` | Russian (25k) | 680 843 | 26 037 | import |
| `russian_375k.json` | `russian_375k` | — | 10 065 189 | 376 092 | skip — deferred by the 60 MB embed budget (10.07 MB, largest remaining) |
| `russian_50k.json` | `russian_50k` | Russian (50k) | 1 354 093 | 51 682 | import |
| `russian_5k.json` | `russian_5k` | Russian (5k) | 112 680 | 4 971 | import |
| `russian_abbreviations.json` | `russian_abbreviations` | Russian (abbreviations) | 3 707 | 245 | import |
| `russian_contractions.json` | `russian_contractions` | Russian (contractions) | 4 028 | 200 | import |
| `russian_contractions_1k.json` | `russian_contractions_1k` | Russian (contractions, 1k) | 15 070 | 880 | import |
| `sanskrit.json` | `sanskrit` | Sanskrit | 5 702 | 252 | import |
| `sanskrit_roman.json` | `sanskrit_roman` | Sanskrit (romanised) | 3 736 | 252 | import |
| `santali.json` | `santali` | Santali | 5 790 | 260 | import |
| `serbian.json` | `serbian` | Serbian | 3 809 | 212 | import |
| `serbian_10k.json` | `serbian_10k` | Serbian (10k) | 207 709 | 10 000 | import |
| `serbian_latin.json` | `serbian_latin` | Serbian (Latin) | 2 849 | 212 | import |
| `serbian_latin_10k.json` | `serbian_latin_10k` | Serbian (Latin, 10k) | 147 387 | 10 000 | import |
| `shona.json` | `shona` | Shona | 2 764 | 200 | import |
| `shona_1k.json` | `shona_1k` | Shona (1k) | 11 959 | 816 | import |
| `sindhi.json` | `sindhi` | Sindhi | 8 456 | 514 | import |
| `sinhala.json` | `sinhala` | Sinhala | 4 190 | 200 | import |
| `slovak.json` | `slovak` | Slovak | 2 361 | 177 | import |
| `slovak_10k.json` | `slovak_10k` | Slovak (10k) | 152 706 | 9 944 | import |
| `slovak_1k.json` | `slovak_1k` | Slovak (1k) | 13 772 | 1 001 | import |
| `slovenian.json` | `slovenian` | Slovenian | 4 027 | 309 | import |
| `slovenian_1k.json` | `slovenian_1k` | Slovenian (1k) | 14 152 | 1 023 | import |
| `slovenian_5k.json` | `slovenian_5k` | Slovenian (5k) | 73 355 | 4 971 | import |
| `spanish.json` | `spanish` | Spanish | 2 627 | 197 | import |
| `spanish_10k.json` | `spanish_10k` | Spanish (10k) | 157 707 | 9 990 | import |
| `spanish_1k.json` | `spanish_1k` | Spanish (1k) | 14 211 | 998 | import |
| `spanish_650k.json` | `spanish_650k` | — | 11 979 087 | 646 579 | skip — deferred by the 60 MB embed budget (11.98 MB, largest remaining) |
| `swahili_1k.json` | `swahili_1k` | Swahili (1k) | 11 674 | 820 | import |
| `swedish.json` | `swedish` | Swedish | 2 462 | 198 | import |
| `swedish_1k.json` | `swedish_1k` | Swedish (1k) | 13 414 | 994 | import |
| `swedish_diacritics.json` | `swedish_diacritics` | Swedish (diacritics) | 4 198 | 261 | import |
| `swiss_german.json` | `swiss_german` | Swiss German | 2 728 | 204 | import |
| `swiss_german_1k.json` | `swiss_german_1k` | Swiss German (1k) | 14 256 | 1 000 | import |
| `swiss_german_2k.json` | `swiss_german_2k` | Swiss German (2k) | 35 726 | 2 000 | import |
| `tamil.json` | `tamil` | Tamil | 9 608 | 366 | import |
| `tamil_1k.json` | `tamil_1k` | Tamil (1k) | 26 672 | 951 | import |
| `tamil_old.json` | `tamil_old` | Tamil (old) | 12 105 | 460 | import |
| `tanglish.json` | `tanglish` | Tanglish | 3 733 | 272 | import |
| `tatar.json` | `tatar` | Tatar | 3 630 | 201 | import |
| `tatar_1k.json` | `tatar_1k` | Tatar (1k) | 19 353 | 1 001 | import |
| `tatar_5k.json` | `tatar_5k` | Tatar (5k) | 105 761 | 5 004 | import |
| `tatar_9k.json` | `tatar_9k` | Tatar (9k) | 196 829 | 9 034 | import |
| `tatar_crimean.json` | `tatar_crimean` | Crimean Tatar | 2 890 | 214 | import |
| `tatar_crimean_10k.json` | `tatar_crimean_10k` | Crimean Tatar (10k) | 159 969 | 10 000 | import |
| `tatar_crimean_15k.json` | `tatar_crimean_15k` | Crimean Tatar (15k) | 241 941 | 15 082 | import |
| `tatar_crimean_1k.json` | `tatar_crimean_1k` | Crimean Tatar (1k) | 15 443 | 1 000 | import |
| `tatar_crimean_5k.json` | `tatar_crimean_5k` | Crimean Tatar (5k) | 79 816 | 5 000 | import |
| `tatar_crimean_cyrillic.json` | `tatar_crimean_cyrillic` | Crimean Tatar (Cyrillic) | 3 966 | 214 | import |
| `tatar_crimean_cyrillic_10k.json` | `tatar_crimean_cyrillic_10k` | Crimean Tatar (Cyrillic, 10k) | 232 342 | 10 000 | import |
| `tatar_crimean_cyrillic_15k.json` | `tatar_crimean_cyrillic_15k` | Crimean Tatar (Cyrillic, 15k) | 351 696 | 15 082 | import |
| `tatar_crimean_cyrillic_1k.json` | `tatar_crimean_cyrillic_1k` | Crimean Tatar (Cyrillic, 1k) | 22 147 | 1 000 | import |
| `tatar_crimean_cyrillic_5k.json` | `tatar_crimean_cyrillic_5k` | Crimean Tatar (Cyrillic, 5k) | 115 925 | 5 000 | import |
| `telugu.json` | `telugu` | Telugu | 4 888 | 201 | import |
| `telugu_1k.json` | `telugu_1k` | Telugu (1k) | 23 369 | 901 | import |
| `thai.json` | `thai` | Thai | 21 316 | 972 | import |
| `thai_10k.json` | `thai_10k` | Thai (10k) | 314 199 | 10 000 | import |
| `thai_1k.json` | `thai_1k` | Thai (1k) | 31 191 | 1 000 | import |
| `thai_20k.json` | `thai_20k` | Thai (20k) | 470 714 | 18 737 | import |
| `thai_50k.json` | `thai_50k` | Thai (50k) | 1 572 943 | 50 000 | import |
| `thai_5k.json` | `thai_5k` | Thai (5k) | 156 845 | 5 000 | import |
| `thai_60k.json` | `thai_60k` | Thai (60k) | 1 888 627 | 60 000 | import |
| `tibetan.json` | `tibetan` | Tibetan | 8 340 | 277 | import |
| `tibetan_1k.json` | `tibetan_1k` | Tibetan (1k) | 40 744 | 1 080 | import |
| `toki_pona.json` | `toki_pona` | Toki Pona | 1 515 | 124 | import |
| `toki_pona_ku_lili.json` | `toki_pona_ku_lili` | Toki Pona (ku lili) | 2 368 | 190 | import |
| `toki_pona_ku_suli.json` | `toki_pona_ku_suli` | Toki Pona (ku suli) | 1 713 | 138 | import |
| `turkish.json` | `turkish` | Turkish | 4 065 | 299 | import |
| `turkish_1k.json` | `turkish_1k` | Turkish (1k) | 14 482 | 1 026 | import |
| `turkish_5k.json` | `turkish_5k` | Turkish (5k) | 77 318 | 5 016 | import |
| `twitch_emotes.json` | `twitch_emotes` | Twitch emotes | 3 220 | 201 | import |
| `typing_of_the_dead.json` | `typing_of_the_dead` | The Typing of the Dead | 232 723 | 10 098 | import |
| `udmurt.json` | `udmurt` | Udmurt | 3 682 | 212 | import |
| `ukrainian.json` | `ukrainian` | Ukrainian | 3 581 | 199 | import |
| `ukrainian_10k.json` | `ukrainian_10k` | Ukrainian (10k) | 244 484 | 9 998 | import |
| `ukrainian_1k.json` | `ukrainian_1k` | Ukrainian (1k) | 21 381 | 1 000 | import |
| `ukrainian_50k.json` | `ukrainian_50k` | Ukrainian (50k) | 1 329 598 | 49 991 | import |
| `ukrainian_endings.json` | `ukrainian_endings` | Ukrainian (endings) | 1 601 | 118 | import |
| `ukrainian_latynka.json` | `ukrainian_latynka` | Ukrainian (Latynka) | 2 699 | 199 | import |
| `ukrainian_latynka_10k.json` | `ukrainian_latynka_10k` | Ukrainian (Latynka, 10k) | 168 668 | 9 998 | import |
| `ukrainian_latynka_1k.json` | `ukrainian_latynka_1k` | Ukrainian (Latynka, 1k) | 15 238 | 1 000 | import |
| `ukrainian_latynka_50k.json` | `ukrainian_latynka_50k` | Ukrainian (Latynka, 50k) | 901 840 | 49 991 | import |
| `ukrainian_latynka_endings.json` | `ukrainian_latynka_endings` | Ukrainian (Latynka, endings) | 1 345 | 117 | import |
| `urdish.json` | `urdish` | Urdish | 2 742 | 201 | import |
| `urdu.json` | `urdu` | Urdu | 3 191 | 200 | import |
| `urdu_1k.json` | `urdu_1k` | Urdu (1k) | 16 064 | 934 | import |
| `urdu_5k.json` | `urdu_5k` | Urdu (5k) | 86 390 | 4 981 | import |
| `urdu_roman.json` | `urdu_roman` | Urdu (romanised) | 2 616 | 200 | import |
| `uzbek.json` | `uzbek` | Uzbek | 2 808 | 196 | import |
| `uzbek_1k.json` | `uzbek_1k` | Uzbek (1k) | 11 743 | 821 | import |
| `uzbek_70k.json` | `uzbek_70k` | Uzbek (70k) | 1 328 976 | 76 595 | import |
| `vietnamese.json` | `vietnamese` | Vietnamese | 4 107 | 319 | import |
| `vietnamese_1k.json` | `vietnamese_1k` | Vietnamese (1k) | 13 005 | 1 000 | import |
| `vietnamese_5k.json` | `vietnamese_5k` | Vietnamese (5k) | 64 648 | 5 000 | import |
| `viossa.json` | `viossa` | Viossa | 2 567 | 210 | import |
| `viossa_njutro.json` | `viossa_njutro` | Viossa (njutro) | 2 897 | 221 | import |
| `welsh.json` | `welsh` | Welsh | 2 507 | 200 | import |
| `welsh_1k.json` | `welsh_1k` | Welsh (1k) | 13 742 | 1 000 | import |
| `wordle.json` | `wordle` | Wordle | 2 653 | 201 | import |
| `wordle_1k.json` | `wordle_1k` | Wordle (1k) | 13 043 | 1 000 | import |
| `xhosa.json` | `xhosa` | Xhosa | 13 178 | 871 | import |
| `xhosa_3k.json` | `xhosa_3k` | Xhosa (3k) | 47 210 | 2 935 | import |
| `yiddish.json` | `yiddish` | Yiddish | 3 728 | 200 | import |
| `yoruba_1k.json` | `yoruba_1k` | Yoruba (1k) | 9 762 | 731 | import |
| `zulu.json` | `zulu` | Zulu | 2 834 | 174 | import |
