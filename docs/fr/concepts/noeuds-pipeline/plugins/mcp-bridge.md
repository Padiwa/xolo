# mcp-bridge

> Plugin livré par défaut, famille « Outils et test ». Retour à la [vue d'ensemble des nœuds](../index.md).

**mcp-bridge** connecte un serveur MCP et expose ses outils au modèle.

## Ports

| Port | Sens | Type | Requis |
| --- | --- | --- | --- |
| `request` | entrée | request | oui |
| `request` | sortie | request | — |

## Configuration

Ce plugin ouvre son propre écran de configuration dans le panneau de droite.

| Champ | Rôle | Défaut |
| --- | --- | --- |
| `endpoint` | URL du serveur MCP |  |
| `authHeaderName` | Nom de l'en-tête d'authentification |  |
| `toolFilter` | Outils autorisés, vide pour tous |  |
| `timeoutSeconds` | Délai maximal en secondes |  |
| `maxConsecutiveToolCalls` | Appels d'outils consécutifs au maximum |  |

## En pratique

Le modèle peut appeler les outils pendant la génération, la passerelle exécute l'appel et renvoie le résultat. Les secrets d'authentification ne sont jamais écrits dans le graphe. Les résultats d'outils sont la surface d'injection indirecte ; [`prompt-guard`](./prompt-guard.md) les inspecte au fil de la boucle quand il est présent dans le pipeline.
