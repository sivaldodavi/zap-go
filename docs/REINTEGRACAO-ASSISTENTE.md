# Reconstrução de integração com o Assistente

Este repositório contém somente o gateway WhatsApp. O Assistente Acadêmico não depende dele: o aplicativo não coleta telefone, não mantém fila de mensagens, não executa worker e não envia notificações automáticas.

Se uma integração voltar a ser desejada, ela deve ser tratada como uma decisão nova, com revisão do código, da política de privacidade e do consentimento. Não reutilize automaticamente configurações antigas nem coloque tokens neste repositório.

## Contrato mínimo a reconstruir

1. Definir no Assistente um consentimento explícito, separado dos termos gerais e da autorização de IA, com telefone validado e retirada de autorização.
2. Criar uma fila transacional no Assistente, com chave idempotente, estados de tentativa, expiração e política para mensagens pendentes. A fila não deve ser o banco do gateway.
3. Implementar um worker separado do processo web, com retry limitado, timeout, logs sem telefone/texto e nenhuma dependência do monitor técnico.
4. Adaptar o cliente do Assistente ao endpoint documentado pelo gateway, usando token somente por secret montado no runtime. Nunca usar o banco, lock ou volume deste gateway como armazenamento do aplicativo.
5. Testar primeiro com transporte falso e destino controlado; nunca disparar mensagem real durante testes automatizados.
6. Atualizar textos públicos, termos e privacidade antes de ativar o envio; documentar claramente boas-vindas, lembretes, alertas e opt-out.
7. Instalar worker/timer somente depois de validar backup, rollback, observabilidade e propriedade dos recursos. O gateway deve continuar independente e não pode ser reiniciado por um deploy do Assistente.

O contato manual por WhatsApp pode existir na vitrine comercial sem constituir integração: nesse caso, a pessoa abre uma conversa externa e o Assistente não lê, sincroniza ou envia mensagens por ela.
