package server

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/ije/esbuild-internal/js_ast"
	"github.com/ije/esbuild-internal/js_parser"
	"github.com/ije/esbuild-internal/logger"
	"github.com/ije/esbuild-internal/test"
	"github.com/ije/gox/utils"
)

var cjsModuleLexerAppDir string

type cjsModuleLexerResult struct {
	Exports []string `json:"exports"`
	Error   string   `json:"error"`
}

func parseCJSModuleExports(buildDir string, importPath string, env string) (ret cjsModuleLexerResult, err error) {
	if cjsModuleLexerAppDir == "" {
		cjsModuleLexerAppDir = path.Join(os.TempDir(), "esmd-cjs-module-lexer")
		ensureDir(cjsModuleLexerAppDir)
		cmd := exec.Command("yarn", "add", "cjs-module-lexer", "enhanced-resolve")
		cmd.Dir = cjsModuleLexerAppDir
		var output []byte
		output, err = cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("yarn: %s", string(output))
			return
		}
	}

	start := time.Now()
	buf := bytes.NewBuffer(nil)
	buf.WriteString(fmt.Sprintf(`
		const fs = require('fs')
		const { dirname, join } = require('path')
		const { promisify } = require('util')
		const moduleLexer = require('cjs-module-lexer')
		const enhancedResolve = require('enhanced-resolve')

		const resolve = promisify(enhancedResolve.create({
			mainFields: ['main']
		}))
		const reservedWords = [
			'abstract*', 'arguments', 'await', 'boolean',
			'break', 'byte', 'case', 'catch',
			'char', 'class', 'const', 'continue',
			'debugger', 'default', 'delete', 'do',
			'double', 'else', 'enum', 'eval',
			'export', 'extends', 'false', 'final',
			'finally', 'float', 'for', 'function',
			'goto', 'if', 'implements', 'import',
			'in', 'instanceof', 'int', 'interface*',
			'let', 'long', 'native', 'new',
			'null', 'package*', 'private', 'protected',
			'public', 'return', 'short', 'static',
			'super', 'switch', 'synchronized', 'this',
			'throw', 'throws', 'transient', 'true',
			'try', 'typeof', 'var', 'void',
			'volatile', 'while', 'with', 'yield',
		]

		// the function 'getExports' is copied from https://github.com/evanw/esbuild/issues/442#issuecomment-739340295
		async function getExports () {
			await moduleLexer.init()

			const exports = []
			const paths = []

			try {
				const jsFile = await resolve('%s', '%s')
				if (!jsFile.endsWith('.json')) {
					paths.push(jsFile) 
				}
				while (paths.length > 0) {
					const currentPath = paths.pop()
					const code = fs.readFileSync(currentPath).toString()
					const results = moduleLexer.parse(code)
					exports.push(...results.exports)
					for (const reexport of results.reexports) {
						if (!reexport.endsWith('.json')) {
							paths.push(await resolve(dirname(currentPath), reexport))
						}
					}
				}
				if (!jsFile.endsWith('.json')) {
					const mod = require(jsFile)
					if (typeof mod === 'object' && mod !== null && !Array.isArray(mod)) {
						for (const key of Object.keys(mod)) {
							if (typeof key === 'string' && key !== '' && !exports.includes(key)) {
								exports.push(key)
							}
						}
					}
				}
				return { exports }
			} catch(e) {
				return { error: e.message }
			}
		}

		getExports().then(ret => {
			const saveDir = join('%s', '%s')
			if (!fs.existsSync(saveDir)){
				fs.mkdirSync(saveDir, {recursive: true});
			}
			if (Array.isArray(ret.exports)) {
				ret.exports = Array.from(new Set(ret.exports)).filter(name => !reservedWords.includes(name))
			}
			fs.writeFileSync(join(saveDir, '__exports.json'), JSON.stringify(ret))
			process.exit(0)
		})
	`, buildDir, importPath, buildDir, importPath))

	cmd := exec.Command("node")
	cmd.Stdin = buf
	cmd.Dir = cjsModuleLexerAppDir
	cmd.Env = append(os.Environ(), fmt.Sprintf(`NODE_ENV=%s`, env))
	output, e := cmd.CombinedOutput()
	if e != nil {
		err = fmt.Errorf("nodejs: %s", string(output))
		return
	}

	err = utils.ParseJSONFile(path.Join(buildDir, importPath, "__exports.json"), &ret)
	if err != nil {
		return
	}

	log.Debug("run cjs-module-lexer in", time.Now().Sub(start))
	return
}

func parseESModuleExports(buildDir string, importPath string) (exports []string, esm bool, err error) {
	var filepath string
	var isImportDir bool
	nmDir := path.Join(buildDir, "node_modules")
	if path.IsAbs(importPath) {
		filepath = importPath
	} else {
		fi, e := os.Lstat(path.Join(nmDir, importPath))
		isImportDir = e == nil && fi.IsDir()
		if isImportDir {
			filepath = path.Join(nmDir, importPath, "index.mjs")
			if !fileExists(filepath) {
				filepath = path.Join(nmDir, importPath, "index.js")
			}
		} else {
			filepath = path.Join(nmDir, importPath)
			if !strings.HasSuffix(filepath, ".js") && !strings.HasSuffix(filepath, ".mjs") {
				filepath = filepath + ".js"
			}
		}
	}
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return
	}
	log := logger.NewDeferLog()
	ast, pass := js_parser.Parse(log, test.SourceForTest(string(data)), js_parser.Options{})
	if pass {
		esm = ast.ExportsKind == js_ast.ExportsESM
		if esm {
			for _, i := range ast.ExportStarImportRecords {
				src := ast.ImportRecords[i].Path.Text
				if isFileImportPath(src) {
					var p string
					if isImportDir {
						p = path.Join(importPath, src)
					} else {
						p = path.Join(path.Dir(importPath), src)
					}
					a, ok, e := parseESModuleExports(buildDir, p)
					if e != nil {
						err = e
						return
					}
					if ok {
						for _, name := range a {
							if name != "default" {
								exports = append(exports, name)
							}
						}
					}
				} else {
					pkgFile := path.Join(nmDir, src, "package.json")
					if fileExists(pkgFile) {
						var p NpmPackage
						err = utils.ParseJSONFile(pkgFile, &p)
						if err != nil {
							return
						}
						if p.Module == "" && p.Type == "module" {
							p.Module = p.Main
						}
						if p.Module == "" && p.DefinedExports != nil {
							v, ok := p.DefinedExports.(map[string]interface{})
							if ok {
								m, ok := v["import"]
								if ok {
									s, ok := m.(string)
									if ok && s != "" {
										p.Module = s
									}
								}
							}
						}
						if p.Module != "" {
							a, ok, e := parseESModuleExports(buildDir, path.Join(src, p.Module))
							if e != nil {
								err = e
								return
							}
							if ok {
								for _, name := range a {
									if name != "default" {
										exports = append(exports, name)
									}
								}
							}
						}
					}
				}
			}
			for name := range ast.NamedExports {
				exports = append(exports, name)
			}
		}
	}
	return
}
