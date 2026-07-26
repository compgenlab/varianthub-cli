#!/usr/bin/env python3
"""convert_csv_to_tab.py — REVEL CSV → tab converter for varhub's build pipeline.

>>> PLACEHOLDER <<< Replace this with the real converter.

varhub runs it as:   python3 convert_csv_to_tab.py <segments>/*.csv | bgzip > {output}
then indexes with:  tabix -s 1 -b 2 -e 2 -S 1 {output}

So this must read the REVEL CSV segment files (given as argv) and write a
tab-delimited stream to stdout whose columns are:

    1: chrom   2: pos (hg38)   3: ref   4: alt   ...   7: REVEL score

with a single header line first (tabix skips it via -S 1). REVEL's hg38 position
is the `grch38_pos` column; rows lacking it should be dropped.
"""
import sys

if __name__ == "__main__":
    sys.exit(
        "convert_csv_to_tab.py is a placeholder — replace it with the real "
        "REVEL CSV→tab converter (see the module docstring for the expected output)."
    )
