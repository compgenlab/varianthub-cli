#!/usr/bin/env python3
"""expand_vep_vcf.py — expand VEP's packed CSQ INFO field into flat INFO tags.

>>> PLACEHOLDER <<< Replace this with the real expander.

VEP `--vcf` packs every consequence field into one comma/pipe-delimited CSQ INFO
value, described by a `##INFO=<ID=CSQ,...Format: A|B|C>` header line. varhub runs
this as the first host post-processing step:

    python3 expand_vep_vcf.py < {workdir}/vep.vcf | ...

So it must read a VCF on stdin and write a VCF on stdout where each CSQ subfield
(Consequence, IMPACT, SYMBOL, …) becomes its own INFO tag, emitting matching
`##INFO` header lines so downstream annotators (and varhub's tool annotations) can
read them directly. Records may carry several CSQ entries (one per transcript);
keep them all — vep_vcf_worst_consequence.py picks among them next.
"""
import sys

if __name__ == "__main__":
    sys.exit(
        "expand_vep_vcf.py is a placeholder — replace it with the real CSQ "
        "expander (see the module docstring for the expected behaviour)."
    )
