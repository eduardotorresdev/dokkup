# Medição

Todo épico entregue pelo executor produz um relatório. Ele não é burocracia: é o único
jeito de saber se o fatiamento foi bom, se o teto de duas rodadas está no lugar certo, e
se o épico estava gordo demais antes de você levantar dez agentes.

Meça enquanto executa. Nenhum número aqui é reconstruível depois: `duration_ms` vem no
retorno de cada agente e some com ele.

## 1. De onde vem cada número

| Número | Fonte exata | Quando registrar |
| --- | --- | --- |
| Tokens por agente | campo `subagent_tokens` do retorno do Agent | ao receber cada entrega |
| Chamadas de ferramenta | campo `tool_uses` do mesmo retorno | idem |
| Duração do agente | campo `duration_ms` do mesmo retorno | idem |
| Ordem de levantamento | sua própria sequência de chamadas | ao levantar |
| Custo das rotinas de teste | `/usr/bin/time -p` sobre cada portão, uma vez, ao fim | no fechamento |
| Tamanho do contexto | `wc -c` dos arquivos lidos ÷ 4 ≈ tokens | no fechamento |
| Linhas entregues | `git diff --numstat` + `wc -l` dos arquivos novos | no fechamento |
| Achados | contagem por classe em cada veredito | a cada validação |

Mantenha a tabela crua num arquivo do scratchpad desde o primeiro agente. Uma linha por
agente: funcionalidade, papel, rodada, tokens, ferramentas, ms.

## 2. Tempo do épico

Duas grandezas diferentes, e confundi-las é o erro mais comum:

- **Tempo-agente** = soma de todos os `duration_ms`. É o que foi gasto, e é o que você
  paga. Não é quanto o épico demorou.
- **Caminho crítico** = a cadeia mais longa de dependências, montada com t=0 no primeiro
  agente. Some as durações em sequência dentro de cada cadeia e tome a maior. É o tempo de
  parede dos agentes.

```
t=0        F1 impl                       → 614
t=614      F1 val ∥ F2 impl ∥ F3 impl    → 1089 / 1322 / 1377
t=1377     F3 val                        → 2001
t=2001     F3 retifica                   → 2318
t=2318     F3 val r2                     → 2804  ← caminho crítico
```

- **Fator de paralelismo** = tempo-agente ÷ caminho crítico. Abaixo de 1.5 o fatiamento
  não valeu o overhead de contrato; acima de 3 verifique se as trilhas estavam mesmo
  isoladas ou se você deixou passar uma dependência.
- **Turnos do orquestrador** não têm duração medida. Conte-os e diga que não foram
  medidos — não estime sem base.

## 3. Tempo por funcionalidade e por frente

Por funcionalidade: some os agentes dela, implementador e validador, todas as rodadas.
Por frente (`back`, `front`, `devops`): some os agentes daquela trilha. Uma trilha que não
existiu se reporta como ausente, nunca como zero — zero sugere que alguém tentou e não
fez.

Separe sempre **implementação** de **validação**. Se a validação passar de metade do
tempo-agente, ou o código está sendo escrito mal, ou os validadores estão fazendo trabalho
de implementador.

Separe também **primeira rodada** de **retificação**. Retificação barata (metade do custo
da primeira rodada, ou menos) é sinal de que o implementador vivo entre rodadas está
pagando por si; retificação cara é sinal de que o contrato estava frouxo.

## 4. Custo das rotinas de teste

Cronometre uma vez, ao fim, com a árvore parada:

```sh
export TIMEFORMAT='%R'
{ time <build> ; } 2>&1 | tail -1
{ time <vet/typecheck> ; } 2>&1 | tail -1
{ time <suíte completa> ; } 2>&1 | tail -1
{ time <lint> ; } 2>&1 | tail -1
# e a suíte de cada pacote tocado, isolada
```

Chame a soma disso de **portão**. O custo total das rotinas é

```
portão × (rodadas de implementação + rodadas de validação) + mutações × suíte-do-pacote
```

Conte as mutações pelos vereditos: todo validador que verificou por mutação diz quantas
rodou.

**Fator de contenção**: a mesma suíte roda 2 a 3 vezes mais devagar quando há três ou
quatro agentes na mesma árvore. Meça sozinho e diga o fator observado nos relatos dos
agentes — é o custo escondido do paralelismo e é ele que decide se vale isolar em
worktrees.

## 5. Tempo e tamanho do contexto

- **Fase de contexto** = do início até o contrato congelado. Conte suas chamadas de
  ferramenta e o volume lido (`wc -c` ÷ 4 ≈ tokens).
- **Contexto de skill** = tamanho da SKILL.md em bytes e palavras.
- **Contexto de contrato** = tamanho do CONTRACT.md, e quantas vezes ele foi lido (uma vez
  por agente levantado).

O contrato é lido por todo agente, então cada palavra dele é multiplicada pelo número de
agentes. Contrato acima de ~1000 palavras para três funcionalidades é sinal de épico gordo.

## 6. Métricas derivadas

| Métrica | Cálculo | Para quê |
| --- | --- | --- |
| Fator de paralelismo | tempo-agente ÷ caminho crítico | o fatiamento valeu? |
| Fração de validação | tempo-agente de validadores ÷ total | o teto de rodadas está certo? |
| Tokens por linha entregue | tokens totais ÷ linhas novas e alteradas | o épico foi caro pelo quê? |
| Custo por achado | tokens de validação ÷ achados | validar ainda paga? |
| Densidade de bloqueante | bloqueantes ÷ rodadas de validação | as instruções positivas estão boas? |
| Achados por rodada | achados r1 vs r2 | a segunda rodada ainda acha coisa? |

Se a segunda rodada parar de achar bloqueante ao longo de vários épicos, o teto pode cair
para uma. Se achar, o teto está certo e o problema está na primeira entrega.

## 7. O que o relatório tem de dizer além de números

- **Desafios**: o que o fluxo não previa e você resolveu na hora.
- **Problemas não previstos**: o que custou tempo e não estava no plano — árvore
  compartilhada, build quebrado por terceiro, achado que cruza fronteira de arquivo.
- **Atividades mais custosas**: ranqueadas por tempo-agente, com o porquê.
- **O que ficou de fora e por quê**, e o que virou issue.
- **Sinais de épico gordo**, conferidos contra a régua abaixo.

## 8. Régua de épico gordo

O limite de funcionalidades por árvore de trabalho é **quatro** — é dele que sai o
agrupamento em worktrees da seção 3.1 da SKILL.md, com grupos de até três.

Um épico está gordo quando dois ou mais destes são verdade:

- mais de quatro funcionalidades, ou qualquer funcionalidade com mais de duas trilhas;
- contrato acima de ~1000 palavras;
- alguma funcionalidade serial que bloqueia mais de duas outras;
- fator de paralelismo abaixo de 1.5;
- fase de contexto acima de ~50k tokens;
- mais de um bloqueante por rodada de validação;
- achado estrutural que cruza fronteira de arquivo entre trilhas — quer dizer que a
  fronteira foi desenhada no lugar errado.

Gordo não é motivo para desistir: é motivo para quebrar em dois épicos, congelar o
contrato entre eles e dar um worktree a cada um.

**Fator de contenção**, medido e registrado a cada execução, é o número que justifica o
worktree: enquanto ele estiver acima de 2×, isolar paga sozinho, sem contar os builds
quebrados e as trocas de branch que ele também evita.

## 9. Onde o relatório vive

Um lugar só: uma **sub-issue** de [ETD-355 — Relatórios de execução do executor](https://linear.app/portile-business-system/issue/ETD-355), no projeto `executor` do
Linear, mais a linha correspondente na tabela da issue índice.

Não escreva cópia no repositório. O relatório não é artefato do código entregue — ele é
artefato da *skill*, e uma execução pode atravessar vários repositórios. Cópia local sai de
sincronia com a issue no primeiro ajuste e passa a mentir sobre a série.

É a série que responde se uma regra desta skill está no lugar certo, e ela só existe se toda
execução registrar a sua.

## 10. Modelo do relatório

```markdown
# <épico> — relatório de execução

Data | escopo de origem (issue/PRD) | branch

## Funcionalidades
tabela: # | funcionalidade | trilhas | divisibilidade (soma e decisão) | rodadas

## Tempo
tempo-agente | caminho crítico | fator de paralelismo | turnos do orquestrador (não medidos)
tabela por funcionalidade, por papel, por frente, por rodada

## Custo das rotinas de verificação
portão medido, comando a comando | nº de execuções | total | fator de contenção

## Contexto
fase de contexto (chamadas, volume) | skill | contrato | vezes lido

## Achados
tabela por rodada: bloqueante / quickwin / estrutural | issues abertas

## Métricas derivadas
as seis da tabela

## Desafios
## Problemas não previstos
## Atividades mais custosas
## Sinais de épico gordo
## O que mudou na skill por causa desta execução
o que a execução ensinou e virou regra — ou, se não virou, por que não
```

A última seção é a que fecha o laço. Uma execução que não mudou nada na skill também diz
isso explicitamente: é a evidência de que as regras estão assentando.

## 11. Derivar os tetos

Os dois tetos da §3.1 da skill — subfuncionalidades por árvore e agrupamento em worktrees —
não são preferência. Cada um tem uma grandeza que o determina, e as três são medidas na
própria execução, sem instrumentação extra.

### 11.1 Teto de subfuncionalidades: contexto do orquestrador

O orquestrador tem uma janela só, e ela precisa sobreviver até o fechamento. Meça o tamanho
do prompt em quatro marcos, lendo o transcript da sessão:

```sh
f=~/.claude/projects/<projeto>/<sessao>.jsonl
jq -r 'select(.message.usage != null)
  | [.timestamp,
     ((.message.usage.cache_read_input_tokens//0)
      + (.message.usage.cache_creation_input_tokens//0)
      + (.message.usage.input_tokens//0))]
  | @tsv' $f
```

Os marcos são: primeira resposta (entrada), primeiro agente levantado (fim da fase de
contexto), último veredito pontuado (fim das ondas), última issue aberta (fechamento). Os
deltas entre eles dão os quatro termos:

| Termo | O que é | Escala com |
|---|---|---|
| `F` | harness + skill carregados | nada |
| `C` | fase de contexto e contrato congelado | tamanho do épico |
| `A` | levantar, receber, pontuar, rodada 2 | número de funcionalidades |
| `G` | fechamento: verificação, issues, relatório | número de achados |

`A` e `G` se dividem pelo número de funcionalidades da execução. `G` é o termo traiçoeiro:
ele não aparece durante as ondas, aparece todo de uma vez na entrega, e é onde a conta
estoura. Some os dois antes de dividir.

```
N* = (0,7 × janela − F − C) / (A + G)
```

A reserva de 30% cobre o que já aconteceu mais de uma vez: rodada 2 que estoura, achado que
obriga a reler um arquivo inteiro.

Medido em dokkup #14 (3 funcionalidades, 51 achados, 12 issues): `F` = 48,0k, `C` = 69,7k,
`A` = 19,8k por funcionalidade, `G` = 24,6k por funcionalidade. Em janela de 1M, `N*` ≈ 13.
Em janela de 200k, `N*` < 1 — ou seja, **abaixo de 1M o executor não fecha uma única
funcionalidade sem compactar no meio**, e a skill não deve ser usada nesse regime sem
aceitar isso conscientemente.

### 11.2 Teto de subfuncionalidades: relógio

Contexto dá folga muito antes do relógio. O que segura o teto na prática é o paralelismo
efetivo `P`, que já sai da §2:

```
P = tempo-agente / caminho crítico
T(N) ≈ T_contexto + (N / P(N)) × T_onda + T_fechamento(N)
```

O critério é marginal, não absoluto: **subir N enquanto a funcionalidade extra devolver mais
de 20% de ganho de relógio**. Medido em #14, `P` = 2,27 para 3 agentes — eficiência de 76%,
já com um quarto do tempo-agente perdido para serialização. A quarta funcionalidade adiciona
uma onda inteira de tempo-agente e devolve fração de onda de relógio; é aí que o teto cai.

O teto vigente é o **menor** entre §11.1 e §11.2.

### 11.3 O que a máquina não decide

É tentador medir a curva de contenção da máquina (K suítes `-race` simultâneas, tempo de
parede) e chamar aquilo de teto. Não é. Uma suíte `-race` consome ~2 núcleos, mas o ciclo de
trabalho de um agente é baixo — em #14, 93,1s de portão dentro de ondas de ~700s, ou ~13%.
O número esperado de suítes simultâneas com N agentes é ≈ 0,13 × N, e numa máquina de 10
núcleos a saturação só chega perto de N=15, muito acima do teto de contexto e do de relógio.

A máquina entra como `P`, não como teto. Medir a curva de contenção custa dezenas de minutos
de CPU e responde uma pergunta que não é a que está sendo feita — não faça isso durante uma
execução.

### 11.4 Teto de worktrees: incidentes de árvore compartilhada

Esse teto não é contexto nem tempo: é isolamento de correção. Conte, no registro de
incidentes da execução, quantas vezes um agente foi atrapalhado por estado que não era dele —
branch trocada debaixo dele, símbolo indefinido vindo de um par no meio da onda, arquivo de
outra sessão no diff. Divida pelo número de agentes-onda.

```
incidentes por agente-onda = incidentes de árvore compartilhada / (agentes × rodadas)
```

Medido em #14: 3 incidentes, 3 agentes numa árvore só, 1 rodada de implementação → **1,0**.

Regra: acima de **0,3**, a próxima execução agrupa em worktrees. Abaixo, árvore única
continua valendo. O indicador cai a zero por construção quando cada grupo ganha árvore
própria, e o custo disso — ~200–500ms e disco por agente — é desprezível perto de uma rodada
perdida por contaminação.

### 11.5 O que registrar no relatório

Três linhas, sempre, para que a série tenha o que comparar:

```
Teto por contexto     N* = ...   (F=..., C=..., A=..., G=..., janela=...)
Teto por relógio      N* = ...   (P=..., ganho marginal da última funcionalidade = ...%)
Incidentes/agente-onda     ...   (teto de worktree: agrupar acima de 0,3)
```
