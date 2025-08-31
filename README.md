# Scan-repo-for-keys

Make sure to open main.go and paste your repo url. 

```
const repoURL = "https://github.com/alon-abadi/sample-keys-repo.git"
```

Then, go run: 
```
go run main.go
```


The script clones your repository in a directory, and also persists the commits it already processed in that directory as an hidden file, `.scan_state.json`

```
       │ File: .scan_state.json
───────┼──────────────────────────────────────────────────────────
   1   │ {
   2   │   "processed": {
   3   │     "02d8ea6ca1d1731143259ecd777614e6fed5fce7": true,
   4   │     "5cd17980bee29896ad71371293ffcaa25e9f09db": true,
   5   │     "81f463871f896ab777a447ef4f978387366b3e48": true,
   6   │     "9d02c750246a11f1ff961f0b57811c5e2b8c52ae": true,
   7   │     "b232b67dbebfbb890d85bc758b1a46d697705602": true,
   8   │     "ce21f8e1bce0ffd2c6108bee8bf9fb4c42cacaa5": true,
   9   │     "e8948d70168ac7118f210d988cdc45de3531719a": true,
  10   │     "f01a0fc11b99373d5e404c34c9e4ddb33086674e": true
  11   │   }
  12   │ }
───────┴──────────────────────────────────────────────────────────
```

If you run it again, it will only process commits set to false. 

If you want to start over, delete this folder and run the script again. 


## Docker: 
You can also run a containarized version with docker. 
Make sure to run docker, and build: 
```
docker build -t aws-secret-scanner .

mkdir -p scan-data

docker run --rm -it -v "$(pwd)/scan-data:/data" aws-secret-scanner
```

(saved state will be inside /scan-data/<repo>/.scan_state.json)