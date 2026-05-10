# Test Fixtures

Pre-built archive and data fixtures for integration and post-processing
tests. These are committed to the repo so tests don't need the creation
tools (e.g. `rar`) at test time — only the extraction tools (`unrar`,
`7z`, `par2`) are required.

## Directories

### `nzb/`
Minimal NZB XML files for parser tests.

### `7z/`
A small `.7z` archive containing `sample.txt`.
`sample.txt.sha256` holds the expected SHA-256 of the extracted file.

### `par2/`
A `data.bin` file with its par2 verification and recovery set.
`data.bin.sha256` holds the expected SHA-256 of the intact file.
Used to test par2 verify and repair operations.

### `split/`
A 3 KB file split into three 1 KB parts (`sample.001`, `.002`, `.003`).
`joined.bin.sha256` holds the expected SHA-256 of the reassembled file.
Used to test the file-join post-processing stage.

### `rar/`
Reserved for a pre-built RAR archive. Creating RAR files requires the
proprietary `rar` binary. If you have it, create a fixture with:

```bash
echo "RAR fixture content" > sample.txt
rar a sample.rar sample.txt
sha256sum sample.txt > sample.txt.sha256
```

### `obfuscated/`
A file with a random hex name (`a8f3b2c1d4e5f6a7.bin`) to test the
deobfuscation pipeline. `expected.sha256` holds its SHA-256.

## Regenerating Fixtures

If you need to regenerate (e.g. after changing test expectations):

```bash
# 7z (requires 7z)
echo "content" > sample.txt && 7z a sample.7z sample.txt

# par2 (requires par2)
par2 create -r10 -n1 data.par2 data.bin

# split (coreutils)
split -b 1024 -d -a 3 source.bin sample.
# then rename .000→.001, .001→.002, .002→.003
```
