"""Bootstrap wrapper for the final two documented RPC repair cases."""
from pathlib import Path
import runpy

here=Path(__file__).parent
source=(here/'finish.py').read_text()
assert source.count('DOCS={988:')==1
source=source.replace('DOCS={988:',"DOCS={994:'D07-codex-pending-on-disconnect.md',988:")
anchor="runner=runner.replace(package_line,'PACKAGES='+repr(packages))"
assert source.count(anchor)==1
addition='''
if ISSUE==994:
    # Preserve the earlier published branch for provenance. This branch is the
    # reviewed version that also proves Worker-level unknown-outcome handling.
    runner=runner.replace("branch=f'fix/{ISSUE}-deep-reliability'", "branch='fix/994-rpc-disconnect-outcome'")
'''
source=source.replace(anchor,anchor+'\n'+addition)
path=here/'finish-reviewed.py'
path.write_text(source)
runpy.run_path(str(path),run_name='__main__')
