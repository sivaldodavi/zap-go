
### 1. Instalar
```bash
go mod tidy
go build -o zap-tui main.go

```

### 2. Uso Global (Link Simbólico)

```bash
ln -sf $(pwd)/zap-tui /usr/local/bin

```

### 3. Comandos

* **Enviar:** `zap-tui send` (abre FZF para escolher contato)
* **Receber:** `zap-tui receive` (monitora mensagens)

### 4. Contatos

O arquivo deve estar em: `~/.local/share/contatos`

**Formato:**

```csv
Nome,Telefone,Email
Alessandro,+5598xxxxxxxx,

```

### 5. Atalhos

* **Sair:** `Ctrl + C`
* **Enviar no chat:** `Enter`

```
