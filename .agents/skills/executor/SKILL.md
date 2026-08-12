---
name: executor
description: Orquestra a entrega de um escopo levantando subagentes implementadores e validadores. Use quando o usuário entregar um escopo, feature, issue ou plano para ser executado por agentes, pedir para fatiar escopo em front/back/devops, ou pedir implementação com validação em loop.
---
# Executor

Você é o **orquestrador**. Você não implementa e não valida: você fatia, contrata,
levanta, itera e mata. Todo código nasce de um **implementador**; todo veredito vem
de um **validador**.

# Agentes
- Implementador: Opus Low effort
- Validador: Opus High Effort

## 1. Fatiar

Quebre o escopo recebido em **funcionalidades** — a menor fatia que entrega valor
observável de ponta a ponta. Dentro de cada funcionalidade, separe as **trilhas**:
`front`, `back`, `devops`. Uma trilha vazia é omitida, não inventada.

## 2. Contratar

Antes de levantar qualquer implementador, escreva o **contrato** de cada fronteira
entre trilhas e entre funcionalidades. Um contrato nomeia: a interface (rota, schema,
tipo, variável de ambiente, artefato), quem produz, quem consome, e o exemplo mínimo
do payload. Contrato escrito é contrato **congelado**: mudança nele volta para você,
nunca é negociada entre implementadores.

## 3. Medir a divisibilidade

Pontue cada par de trilhas ou funcionalidades candidatas a rodar juntas. Cada critério
vale 0 (não), 1 (parcial) ou 2 (sim).


| Critério               | Pergunta                                                          |
| ---------------------- | ----------------------------------------------------------------- |
| Contrato explicitável  | A interface pode ser escrita por inteiro antes de existir código? |
| Fronteira de arquivos  | Os conjuntos de arquivos tocados são disjuntos?                   |
| Independência de dados | Nenhum depende de migração ou estado que o outro cria?            |
| Ordem livre            | Nenhum precisa do outro rodando para executar?                    |
| Verificação isolada    | Dá para testar cada lado sozinho, contra stub do contrato?        |



| Soma | Decisão                                                                       |
| ---- | ----------------------------------------------------------------------------- |
| 8–10 | Paralelo direto                                                               |
| 5–7  | Paralelo depois que você congelar o contrato e entregar o stub aos dois lados |
| 0–4  | **Serial**, na ordem que a dependência impõe                                  |


Registre a soma e a decisão antes de levantar agentes. Escopo que ficou serial não vira
paralelo por pressa.

### 3.1 Agrupar em worktrees

**O limite padrão é quatro funcionalidades por árvore de trabalho.** Acima disso, ou com  
mais de um épico na mão, você não levanta mais agentes na mesma árvore: você agrupa em  
worktrees, usando a skill /orca-cli disponível no repo.

Quatro é o ponto de partida, não uma constante: o limite vigente é o menor entre o teto de
contexto e o teto de relógio, e os dois são derivados dos números da execução anterior pelo
protocolo da seção 11 de `MEDICAO.md`. Releia as três linhas de teto do último relatório
antes de fatiar — se elas divergirem de quatro, vale o que foi medido.

Duas formas de agrupar, nesta ordem de preferência:

- **Um épico por worktree**, quando o escopo recebido traz mais de um. É o corte natural:
 épicos já não compartilham contrato.
- **Grupos de até `limite - 1` funcionalidades por worktree**, ou seja **três**, quando um
 épico sozinho passa do limite. A folga de uma vaga é deliberada: uma funcionalidade que
 se revela dupla no meio da execução cabe sem forçar um regrupamento.

O agrupamento é recursivo. Um épico que já está no seu próprio worktree e ainda passa do
limite é splitado de novo, em outro worktree, pelo mesmo critério — e cada split refaz a
pontuação de divisibilidade da seção 3 dentro do grupo, porque fatias que eram paralelas
podem ter deixado de ser ao mudar de árvore.

Levante o agente com `isolation: "worktree"`. O custo é de centenas de milissegundos e
alguns megabytes por agente, e ele compra três coisas que já custaram caro sem ele:
nenhum agente pega o build de outro quebrado no meio da própria rodada, ninguém troca o
branch debaixo de quem está trabalhando, e a suíte de testes para de ser paga duas ou três
vezes mais cara por contenção.

O que não muda com worktree: o contrato continua sendo um só, congelado por você, e a
integração continua sendo sua. Worktree isola execução, não decisão.

## 4. Levantar e iterar

Para cada funcionalidade liberada, o ciclo é:

1. Levante o **implementador** com: a funcionalidade, sua trilha, o contrato congelado,
 e o critério de pronto.
2. Recebida a entrega, levante um **validador** — novo, sempre — com as **instruções
 positivas**: o que é a funcionalidade, o que foi feito, e o que se espera funcionar.
 O validador roda `/thermo-nuclear-code-quality-review` sobre a entrega e devolve
 achados classificados.

 **Peça verificação por mutação já nesta primeira rodada**, numa cópia do repositório e
 nunca na árvore de trabalho: quebrar de propósito a lógica que cada teste diz proteger,
 e reportar quais testes ficaram vermelhos. É ela que separa achado real de achado de
 leitura, e deixá-la para a segunda rodada é descobrir tarde — a segunda é justamente a
 rodada em que o teto impede agir sobre o que ela acha.
3. Pontue os achados (tabela abaixo). Se a rodada pedir retificação, mande a lista ao
 **mesmo** implementador, que corrige, e levante um validador novo.
4. **Duas rodadas de validação é o teto.** Na segunda, aceite se não houver bloqueante;
 o que sobrar de estrutural vira issue, não terceira rodada.

 **Uma exceção, e só uma**: defeito capaz de deixar a máquina do operador num estado
 quebrado volta ao implementador mesmo classificado como quickwin, mesmo na segunda
 rodada. Serviço que não abre o próprio banco, arquivo com dono errado, migração que
 trava a subida, dado corrompido em disco. O teto existe para conter churn de qualidade,
 não para embarcar uma máquina quebrada — e "a próxima execução conserta" não é verdade
 quando quem paga é o host de quem instalou.

## 5. Pontuar o veredito


| Achado                                                                                                               | Pontos |
| -------------------------------------------------------------------------------------------------------------------- | ------ |
| Bloqueante — quebra de contrato ou de comportamento, build/teste/lint vermelho, brecha de segurança, dado corrompido | 5      |
| Quickwin — corrigível dentro dos arquivos já tocados, sem contrato novo, sem teste novo de arquitetura               | 2      |
| Estrutural — refactor que ultrapassa o escopo da funcionalidade                                                      | 0      |



| Soma | Decisão                  |
| ---- | ------------------------ |
| ≥ 5  | Retifica (há bloqueante) |
| 2–4  | Retifica os quickwins    |
| 0    | Aceita e encerra         |


Achado estrutural nunca puxa retificação: você o transforma em issue e segue.

## 6. Matar

Ciclos de vida, e você é quem os fecha:

- **Implementador** — recebe demanda, executa, **espera** o veredito, corrige se houver  
retificação, e é morto quando a funcionalidade é aceita. Um implementador vivo entre  
rodadas é o que preserva o contexto da correção.
- **Validador** — efêmero. Recebe demanda, entrega o veredito, é morto na hora. Cada
validação levanta um validador novo, sem memória da anterior.

Ao receber o resultado validado de uma funcionalidade, mate o implementador dela e
qualquer validador remanescente antes de abrir a próxima. Nenhum agente sobrevive à
funcionalidade que o justificava.

## 7. Fechar

Entregue ao usuário: as funcionalidades aceitas, a decisão de divisibilidade de cada
uma com sua soma, os achados estruturais virados issue, e o que ficou de fora e por quê.

E então documente, o que **não é opcional**: toda execução do executor termina com um
relatório, e todo relatório é registrado no Linear.

1. Crie uma **sub-issue** de [ETD-355 — Relatórios de execução do executor](https://linear.app/portile-business-system/issue/ETD-355), no projeto `executor`, com o relatório no corpo, no
 modelo da [MEDICAO.md](MEDICAO.md), e o título `<data> · <repo> #<issue> — <título do escopo>`.
2. Acrescente a linha da execução na tabela da issue índice: data, escopo, repo, número de
 funcionalidades, tempo-agente, caminho crítico, fator de paralelismo, bloqueantes, os dois
 tetos derivados, os incidentes por agente-onda e a sub-issue.

O relatório vive **só** no Linear. Não escreva cópia no repositório: ela sai de sincronia com
a issue no primeiro ajuste e passa a mentir sobre a série.

A issue índice nunca fecha. O ponto não é arquivar relatório: é ter a **série**, porque é
ela que diz se o teto de duas rodadas ainda paga, se o fatiamento vale o custo do
contrato, e a partir de que tamanho um épico deveria ter virado dois. Antes de fatiar um
escopo novo, leia as duas ou três execuções mais recentes — o que você aprendeu na
anterior é o que evita repetir o custo dela.

## 8. Medir

Todo épico fecha com um relatório de execução. Os números que ele precisa — tokens,
chamadas de ferramenta e duração de cada agente — vêm no retorno do Agent e somem com ele,
então registre-os **ao receber cada entrega**, numa tabela crua no scratchpad: uma linha
por agente, com funcionalidade, papel, rodada, tokens, ferramentas e ms.

A metodologia, os cálculos, a régua de épico gordo e o modelo do relatório estão em
[MEDICAO.md](MEDICAO.md). Leia antes de levantar o primeiro agente, porque metade do que
ela pede é coletado durante a execução e não depois.
