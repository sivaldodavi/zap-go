
### Pré-requisitos
* **Go 1.18+** instalado.
* Uma conta no **Disroot.org** (ou outro provedor SMTP).

### Instalação e Build

1. Acesse o diretório do projeto:
   ```bash
   cd ~/.local/dev/zap-go
   ```

2. Sincronize as dependências:
   ```bash
   go mod tidy
   ```

3. Compile o executável:
   ```bash
   go build -o zap .
   ```

---

## Configuração
O sistema utiliza um arquivo de configuração chamado `config.toml`. Por questões de segurança, este arquivo **não deve ser enviado ao Git** (já incluído no `.gitignore`).

Crie o arquivo na raiz do projeto:
```bash
touch config.toml
```

E adicione o seguinte conteúdo:

```toml
[smtp]
# Servidor de email — para Disroot é sempre este
host = "disroot.org"

# Porta — tente 587 primeiro. Se não funcionar, tente 465
port = 587

# Seu email completo
user = "user@disroot.org"

# Sua senha do Disroot
password = "sua_senha_aqui"
```

---

## 🛠️ Execução

Após realizar o build e configurar o `.toml`, basta rodar o binário:

```bash
./zap
```
