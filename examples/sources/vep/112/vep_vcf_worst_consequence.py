#!/usr/bin/env python3
"""vep_vcf_worst_consequence.py — collapse per-transcript CSQ to the worst hit.

>>> PLACEHOLDER <<< Replace this with the real picker.

A variant often has several transcript consequences. varhub runs this as the second
host post-processing step, right after expand_vep_vcf.py:

    ... | python3 vep_vcf_worst_consequence.py | varhub bgzip > {output}

So it must read a VCF on stdin (with the per-transcript INFO tags produced by
expand_vep_vcf.py) and write a VCF on stdout carrying a single value per INFO tag —
the most severe consequence per record — using the standard VEP severity ranking
(https://www.ensembl.org/info/genome/variation/prediction/predicted_data.html).
The one-value-per-tag output lets each [[tools.annotations]] `field` be read as a
plain INFO id.
"""
import sys

if __name__ == "__main__":
    sys.exit(
        "vep_vcf_worst_consequence.py is a placeholder — replace it with the real "
        "worst-consequence picker (see the module docstring for the expected behaviour)."
    )
