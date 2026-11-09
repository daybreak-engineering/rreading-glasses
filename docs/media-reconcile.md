# Media reconciliation record

What the rreading-glasses work changed outside this repo, from 2026-11-07
on, for reconciling into the unified media management session. Times are
UTC. Append to this file whenever a Bookshelf, curation or cluster change is
made. No secrets belong here.

Background on the library cleanup before this work (the 1,029 sibling
deletions, canon lists, edition fixes): `tmp/junk-metadata.md`.

## Sources of truth

| What | Where |
|---|---|
| Every curation change, with before/after | `curation_log` in the store: MCP `get_curation_log`, filter by the `source` tags below |
| Bookshelf deletions | Bookshelf log, `RefreshBookService` warnings, and the tables below |
| Library snapshot before the import (2026-11-09) | `tmp/import/ebook-books.json`, `tmp/import/ebook-exclusions.json` (gitignored) |
| Import plan and dry-run report | `tmp/import/plan.json`, `plan-notes.json`, `report.json` (gitignored) |
| Review of the 213 unmonitored books with files | `tmp/import/unmonitored-review.tsv` (gitignored) |
| Files left on disk | `tmp/import/orphaned-files.txt` (gitignored; copied below) |

## Current state (2026-11-09)

- `SERVE_CURATED=true` in the cluster: curation affects author lists, work
  and edition lookups, and search.
- Kurt Vonnegut (Hardcover 149651, Bookshelf author 1) is the only author in
  `allowlist` mode. Everyone else is in `rules` mode.
- Audio Bookshelf (`bookshelf-audio.xor86.io`) had 0 books and 0 exclusions
  when read on 2026-11-09, but it uses this server too.

## Curation written to the store

| When | `source` | Change |
|---|---|---|
| 2026-11-08 22:56 | `test:merge-follow` | Merged 3001607 into 363685, for a test |
| 2026-11-09 ~00:50 | `test:merge-follow` | Merge cleared; an empty `work_curation` row remains |
| 2026-11-09 15:08 | `import:bookshelf-library` | 3,404 works: 2,356 canon (monitored books), 859 of those with `edition_id` (ebook instance's monitored edition), 1,048 hidden (import-list exclusions) |
| 2026-11-09 15:2x | `import:unmonitored-with-files` | 201 works canon, 12 hidden |
| 2026-11-09 15:29 | `pilot:allowlist` | Vonnegut set to `allowlist` |

Rollback: `update_work(clear=[...])`, `set_works_status(status="clear")`,
`set_author_mode(mode="rules")`, working from the log.

## Bookshelf changes (ebook instance)

### Test entry, added and removed

Book 14619 (work 3001607, *Fates Worse Than Death: An Autobiographical
Collage*), unmonitored, no files, added 2026-11-08 for the merge test and
deleted 2026-11-09 with no exclusion. Net change: none.

`RefreshAuthor` ran for Vonnegut (author 1) on 2026-11-09 at 00:02, 00:40
and 15:29.

### 12 entries deleted by hand, 2026-11-09 ~15:25

`delete_files=false`, import-list exclusion added; each work is also
`hidden` in the store. The file is still on disk.

| Book | Work | Title | Why | Kept copy |
|---|---|---|---|---|
| 4164 | 3049733 | A Chritmas Carol | Duplicate, same size as kept | 4169 |
| 4735 | 107523 | Connecticut Yankee at king Arthur's court. | Duplicate; file 11.9 MB vs 0.8 MB | 4988 |
| 5260 | 1987417 | Moby Dick Or, The Whale | Duplicate | 5274 |
| 5298 | 2962155 | The ConfidenceMan His Masquerade | Duplicate | 5228 |
| 8340 | 379151 | [The Canterbury Tales (store: prologue) | Duplicate; file 17.6 MB vs 1.2 MB | 8359 |
| 9390 | 257864 | Alice's Adventures in Wonderland | Duplicate; 39 MB azw3 vs 27 MB epub | 9460 |
| 13898 | 1713900 | Black Boy | Duplicate | 13872 |
| 7999 | 428657 | Bram Stoker's Dracula | Duplicate; file 0.2 MB vs 1.0 MB | 8017 |
| 13206 | 2818404 | BAF 20 - The Well at World's End | Duplicate | 13155 |
| 559 | 2657946 | Pride and Prejudice, Annotated (retold for young adults) | Retelling | 536 |
| 1764 | 1313842 | Darcy Swipes Left | Fan work | none |
| 3687 | 2954310 | asimov 2 | Unidentified | none |

### 28 entries deleted by Bookshelf, 2026-11-09 15:29:34-44

Caused by the Vonnegut allowlist switch and refresh. All were unmonitored
with no files (per the baseline taken just before). Log: "not found in
metadata and is being deleted". No exclusions were added.

| Book | Work | Title |
|---|---|---|
| 666 | 468309 | The Kid Nobody Could Handle |
| 669 | 470490 | Sinbad |
| 670 | 473657 | 2BR02B |
| 673 | 496439 | Harrison Bergeron |
| 677 | 761425 | Novels 1976-1985 |
| 678 | 781771 | The Big Trip Up Yonder |
| 679 | 788678 | Novels & Stories 1963-1973 |
| 682 | 878474 | Kurt Vonnegut: The Last Interview |
| 683 | 878476 | Kurt Vonnegut: Novels & Stories 1950-1962 |
| 684 | 1064910 | Basic Training |
| 685 | 1128079 | Tomorrow and Tomorrow and Tomorrow |
| 686 | 1132866 | Cat's Cradle/God Bless You Mr. Rosewater/Breakfast of Champions |
| 687 | 1284327 | FUBAR |
| 688 | 1284328 | The Petrified Ants |
| 689 | 1366337 | Canary in a Cat House |
| 690 | 1400072 | Hello, Red |
| 691 | 1400089 | Little Drops of Water |
| 692 | 1400100 | The Nice Little People |
| 693 | 1494173 | Three by Vonnegut |
| 694 | 1529325 | Selected Works of Kurt Vonnegut |
| 695 | 1934101 | Kurt Vonnegut on Mark Twain, Lincoln, Imperialist Wars and the Weather |
| 697 | 2045752 | Slice of Life |
| 698 | 2077038 | Long Walk to Forever |
| 699 | 2109828 | Report on the Barnhouse Effect |
| 701 | 2370612 | Slaughterhouse-Five, or the Children's Crusade (graphic novel) |
| 703 | 2901006 | Kurt Vonnegut, Jr. Audio Collection |
| 704 | 3021671 | Speaking Of Slapstick |
| 705 | 3077007 | Slaughterhouse Five/ The Sirens Of Titan/ ... (omnibus) |

Vonnegut after: 38 books (was 66), 36 monitored, 22 with files, both
unchanged. To bring any back: mark the work canon, then `add_book`
unmonitored. Survivors outside canon: 657 *If God Were Alive Today* and 658
*Monkey Business*, both `manualAdd: true`.

## Cluster and local changes

| Item | Value |
|---|---|
| Image, phase 1 (3a227a7) | `sha256:580e59c0c7387251b91c2f983827829d318e7935b8f853f6f0c0c58691517253` |
| 58ed093 (deadlock fix, admin API) | `sha256:7faefaa09b3296bf9d21263b3b7970a6456c42e6e479b69e986705285155bd31` |
| cf22a30 (dry runs) | `sha256:e8d63a89809b7f856870456be6db20f814a383a4669c6c452929ad7b8b63a497` |
| 192396f (curated authors and works) | `sha256:7014b615d52e8db39b94007b5616d08d4dbafd804c3e76a674c8c1836495b5f1` |
| 90d8bde (curated search and lookups), running | `sha256:89783deb4d97384b60ca2bb0a6fdcfa94517d69dd4094b7d27dc3f5ec50286b0` |
| `SERVE_CURATED=true` | k8s-cluster `ac33f34` |
| Admin API | `https://rreading-glasses-admin.xor86.io`, LAN-only, service port 8789 |
| Admin token | Secret `bookshelf/rreading-glasses-admin`; plaintext copy `k8s-cluster/tmp/rreading-glasses-admin-token` |
| MCP | `rreading-glasses`, user scope, registered by `tmp/mcp-add.sh` (holds both Bookshelf API keys in plaintext, mode 0700) |

## What we learned about Bookshelf

- **A book that leaves an author's list is deleted on refresh unless it has
  a manually added edition.** 28 entries went this way; the 2 survivors had
  `manualAdd: true`, as did the merge-test book, which was added by POST.
  The deleted entries' flags weren't captured, so this part is inferred. A
  surviving book keeps refreshing through `/work/<id>`.
- **Merges aren't followed.** The merged entry stays, or is deleted by the
  rule above, and the target doesn't gain the edition.
- **A book's monitored edition survived refreshes** even when it differs
  from the served default (book 665 only).
- `IsEbook` is true for `eBook` from 2026-11-08, spreading as entries
  refresh. Effect on Bookshelf unverified.

## Open items to reconcile

- [ ] Rolling out allowlist deletes each author's non-canon, fileless,
  non-manual entries. Decide if that's wanted author by author. The MCP
  guard doesn't yet report these deletions.
- [ ] 12 orphaned files, below: purge or re-import. A rescan may pick them
  up as unmapped files. Some are larger than the kept copy, so check first.
- [ ] 6 older unmapped files: Notes on slaughterhouse five, Puppet Masters,
  Nick Adams Stories, The Iliad - Illustrated, The Two Towers - The Ring
  Goes East, Twelfth Night.
- [ ] Foreign-language files kept as canon: Sapiens (book 14151, French),
  Pylône (5912, French; `Pylon` 5881 is monitored with no file), Gogol
  (7342, Russian), Feeling Good (14185, Russian), Never Eat Alone (14516,
  Ukrainian).
- [ ] *Mary Shelley's Frankenstein* (7895) has a 13 MB file; monitored
  *Frankenstein* (7991) has none. Check the file before moving it.
- [ ] Files on minor duplicate works, main work not in the library: The
  Three-Body Problem (14132, main 208339), Man's Search for Meaning (14138,
  main 495645), Beyond Good and Evil (14528, main 482336).
- [ ] *The Way of Kings* (386446) was missing from Sanderson's served list
  despite being monitored with a file; canon now adds it.
- [ ] Pride and Prejudice (536) showed two files on 2026-11-09.
- [ ] Rotate or remove the plaintext keys in `tmp/mcp-add.sh` when done.

## Orphaned files

```
/data/tank/media/books/Charles Dickens/A Christmas Carol - The Original 1843 Edition/Charles Dickens - A Christmas Carol - The Original 1843 Edition.epub
/data/tank/media/books/Mark Twain/Connecticut Yankee at king Arthur's court/Mark Twain - Connecticut Yankee at king Arthur's court.epub
/data/tank/media/books/Herman Melville/Moby Dick Or, The Whale/Herman Melville - Moby Dick Or, The Whale.epub
/data/tank/media/books/Herman Melville/The ConfidenceMan His Masquerade/Herman Melville - The ConfidenceMan His Masquerade.epub
/data/tank/media/books/Geoffrey Chaucer/[The Canterbury Tales/Geoffrey Chaucer - [The Canterbury Tales.epub
/data/tank/media/books/Lewis Carroll/Alice's Adventures in Wonderland - LEWIS CARROLL/Lewis Carroll - Alice's Adventures in Wonderland - LEWIS CARROLL.azw3
/data/tank/media/books/Richard Wright/Black Boy/Richard Wright - Black Boy.epub
/data/tank/media/books/Bram Stoker/Bram Stoker's Dracula/Bram Stoker - Bram Stoker's Dracula.epub
/data/tank/media/books/Jane Austen/Pride and Prejudice, Annotated - Jane Austen's Timeless Classic - A Captivating Love Story of Darcy and Elizabeth Retold for Young Adults/Jane Austen - Pride and Prejudice, Annotated - Jane Austen's Timeless Classic - A Captivating Love Story of Darcy and Elizabeth Retold for Young Adults.epub
/data/tank/media/books/Jane Austen/Darcy Swipes Left/Jane Austen - Darcy Swipes Left.epub
/data/tank/media/books/Isaac Asimov/asimov 2/Isaac Asimov - asimov 2.epub
/data/tank/media/books/William Morris/BAF 20 - The Well at World's End/William Morris - BAF 20 - The Well at World's End.epub
```
