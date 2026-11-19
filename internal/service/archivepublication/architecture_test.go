package archivepublication

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Exact function entries, never package/file wildcards. Each exception must
// explain why this operation cannot expose or mutate a ZIP source generation.
var archiveEngineExemptions = map[string]string{
	"admin/user_delete.go:userDeletionData.deleteExternalData":                                   "Account erasure must delete every generation, including unpublished and orphaned rows; never returns content.",
	"agentic_rag/compiled_evidence.go:readCompiledClaimsByChunk":                                 "Filter entity_type_kwd=claim and scope_kwd=doc; ZIP pipeline never emits claims.",
	"agentic_rag/compiled_evidence.go:searchClaimRows":                                           "Private claim-search executor; sole production caller pins entity_type_kwd=claim and scope_kwd=doc.",
	"agentic_rag/compiled_knowledge.go:datasetsHaveCompiledKnowledge":                            "Existence probe requires compile_kwd; ZIP source rows have none.",
	"agentic_rag/compiled_knowledge.go:datasetsHaveClaims":                                       "Existence probe requires entity_type_kwd=claim; not ZIP source.",
	"agentic_rag/compiled_serve.go:fetchCompiledProducts":                                        "buildCompiledFilter requires compile_kwd and source_chunk_ids; only compiler products.",
	"agentic_rag/kg_service.go:KgAdapter.graphRowsExist":                                         "Requires knowledge_graph_kwd=entity; not source chunks.",
	"agentic_rag/kg_service.go:KgAdapter.kgSearchRaw":                                            "Private KG executor pins entity/relation row kinds, not source passages (loadPassages is guarded separately).",
	"agentic_rag/tool_navigate_structure.go:engineStructureReader.ReadStructure":                 "Requires knowledge_graph_kwd in shapeKwds; ZIP source has no graph role.",
	"handler/dataset.go:DatasetsHandler.GetKnowledgeGraph":                                       "Requires knowledge_graph_kwd=graph; no source reads.",
	"handler/dataset.go:DatasetsHandler.DeleteKnowledgeGraph":                                    "Deletes only explicit graph/subgraph/entity/relation/community roles, never ZIP source.",
	"ingestion/knowledge_compile/reader.go:engineReader.LoadDocProducts":                         "Only reconstructs rows carrying recognized compile_kwd; source rows are discarded, not exposed.",
	"ingestion/knowledge_compile/reader.go:engineReader.LoadMergedProduct":                       "Dataset-scope compiled product reconstructed through productFromChunkMap, which rejects non-Wiki rows.",
	"ingestion/knowledge_compile/reader.go:engineReader.LoadMergedWikiPages":                     "Filter scope_kwd=dataset and type_kwd=wiki_page.",
	"ingestion/knowledge_compile/reader.go:engineReader.LoadDocumentWikiPagesBySlugs":            "Filter type_kwd=wiki_page; loads compiler staging pages, not source.",
	"ingestion/knowledge_compile/reader.go:engineReader.SearchSimilar":                           "Requires compile_kwd matching variant; validates row family before returning.",
	"ingestion/knowledge_compile/wiki_contribution.go:engineWikiContributionStore.Get":           "Reads deterministic wiki contribution state ID with its explicit type_kwd.",
	"ingestion/knowledge_compile/wiki_contribution.go:engineWikiContributionStore.Delete":        "Deletes deterministic wiki contribution state ID, disjoint from source chunk IDs.",
	"ingestion/knowledge_compile/wiki_contribution.go:engineWikiContributionStore.ClearDataset":  "Deletes only wiki contribution state type_kwd and available_int=0.",
	"ingestion/knowledge_compile/wiki_map_store.go:wikiMapVersionStore.GetWikiMapActiveState":    "Requires type_kwd=wiki_map_active and state ID; not source content.",
	"ingestion/knowledge_compile/wiki_map_store.go:wikiMapVersionStore.GetWikiMapVersions":       "Requires type_kwd=wiki_map_extract and version IDs; not source content.",
	"ingestion/knowledge_compile/writer.go:engineWriter.DeleteMergedWikiPages":                   "Requires dataset scope and wiki_page role before deleting supplied IDs.",
	"ingestion/knowledge_compile/writer.go:engineWriter.loadWikiPageContent":                     "Queries wiki_page role; GetChunk hydrates only IDs returned by that query.",
	"ingestion/knowledge_compile/writer.go:engineWriter.WriteMergedStructure":                    "Deletes only stale dataset_structure IDs returned by mergeExistingStructureBuckets.",
	"ingestion/knowledge_compile/writer.go:mergeExistingStructureBuckets":                        "Requires dataset scope, compile kind and graph entity/relation roles.",
	"ingestion/knowledge_compile/writer.go:engineWriter.DeleteStructureForDocs":                  "Scans only dataset graph roles; deletes those IDs and kg_build_meta markers.",
	"ingestion/knowledge_compile/writer.go:engineWriter.DeleteDocLevelForDocs":                   "Deletion consumer purges all rows of deleted documents; filtering publication would leave stale generations.",
	"ingestion/knowledge_compile/writer.go:engineWriter.StripMergedSources":                      "Only compiler products carry source_doc_ids; updates/deletes provenance links, never exposes source content.",
	"ingestion/knowledge_compile/writer.go:engineWriter.loadActiveDocumentWikiPages":             "Requires wiki_page role, doc scope and hidden compiler staging state.",
	"ingestion/knowledge_compile/writer.go:engineWriter.DeleteMergedForVariant":                  "Deletes compiler dataset scope or explicit Wiki/navigation/graph roles; ZIP source has none.",
	"ingestion/knowledge_compile/writer.go:engineWriter.dropWikiGraph":                           "Deletes only explicit Wiki graph compile kinds.",
	"ingestion/knowledge_compile/writer.go:engineWriter.loadMergedWikiPages":                     "Requires wiki_page role and merged availability.",
	"ingestion/service/archive_cleanup.go:Ingestor.startArchiveCleanup":                          "Cleanup callback runs under publication revocation/lease fencing and deletes exact doc/run/attempt, including invisible rows.",
	"ingestion/task/knowledge_compiler_wiring.go:replaceDirtyWikiProducts":                       "Queries wiki_page/wiki_section staging roles and deletes only their superseded IDs.",
	"ingestion/task/knowledge_compiler_wiring.go:clearWikiActiveStates":                          "Deletes only type_kwd=wiki_map_active with source document scope.",
	"ingestion/task/knowledge_compiler_wiring.go:kcWikiPageStore.FindSimilarPages":               "Requires type_kwd=wiki_page, not source chunks.",
	"ingestion/task/knowledge_compiler_wiring.go:kcWikiPageStore.GetPageBySlug":                  "Requires type_kwd=wiki_page and slug.",
	"ingestion/task/knowledge_compiler_wiring.go:kcWikiPageStore.FindPagesBySourceChunks":        "Requires type_kwd=wiki_page; source_chunk_ids selects lineage, not source rows.",
	"ingestion/task/knowledge_compiler_wiring.go:kcWikiPageStore.loadPageCandidate":              "Private hydrator accepts IDs from the three wiki_page-only methods above.",
	"ingestion/task/pipeline_executor.go:NewPipelineExecutor":                                    "Binds cleanup callback; CompensateArchive fences revocation and supplies immutable generation predicate before invoking it.",
	"ingestion/task/pipeline_executor.go:PipelineExecutor.loadDocumentCompiledState":             "Projects only compilation identifiers; discards rows without compile_kwd before collecting IDs.",
	"ingestion/task/pipeline_executor.go:PipelineExecutor.reconcileDocumentCompiledProducts":     "Deletes only old compiled IDs collected by loadDocumentCompiledState; archive execution uses its own publication branch.",
	"service/chat_session.go:ChatSessionService.updateChunkWeight":                               "Feedback modifies ranking metadata only, including atomic adjuster path; no content leaves function and availability/generation fields never change.",
	"service/chunk/chunk.go:ChunkService.UpdateChunk":                                            "Rejects code_archive documents with ZIP_CHUNKS_READ_ONLY before engine read/write.",
	"service/chunk/chunk.go:ChunkService.RemoveChunks":                                           "Rejects code_archive documents with ZIP_CHUNKS_READ_ONLY before deletion.",
	"service/chunk/chunk.go:ChunkService.nextSourceChunkOrder":                                   "Private AddChunk helper; AddChunk rejects ZIP before calling; reads only order metadata.",
	"service/connector.go:ConnectorService.deleteConnectorDocumentChunks":                        "Connector deletion purges complete removed documents; no content returned and ZIP connector upload is prohibited.",
	"service/dataset/crud.go:DatasetService.deleteDatasetEngineData":                             "Caller RevokeDataset fences workers first; purge must remove unpublished and stale rows too.",
	"service/dataset/update.go:DatasetService.updateDatasetPagerankChunks":                       "Dataset-wide ranking metadata update intentionally covers all generations; cannot publish or expose content.",
	"service/dataset_artifact_service.go:DatasetArtifactService.searchCompiledWithMatch":         "Private artifact executor; callers supply explicit Wiki/skill compile roles, never raw source queries.",
	"service/dataset_artifact_service.go:loadWikiPageBody":                                       "Hydrates an ID from wiki_page-only artifact search, not user-supplied source IDs.",
	"service/dataset_artifact_service.go:DatasetArtifactService.UpdateWikiPage":                  "Resolves ID through wiki_page role and slug before mutation.",
	"service/dataset_artifact_service.go:DatasetArtifactService.DeleteDocumentGraph":             "Collects IDs of document graph products through explicit graph roles before deleting.",
	"service/dataset_artifact_service.go:DatasetArtifactService.DeleteSkills":                    "Collects skill compilation product IDs before deleting, not source chunks.",
	"service/dataset_structure_graph.go:graphRowSearch":                                          "Private structure executor; all callers constrain graph roles/scope, not ZIP source rows.",
	"service/document/document_artifact.go:DocumentService.imageDocumentID":                      "Image ownership lookup requires exact img_id; ZIP parser emits no images or image IDs.",
	"service/document/document_crud.go:DocumentService.deleteDocEngineData":                      "Lifecycle purge must delete all rows, not only published ones; no source content returned.",
	"service/document/document_dataset_update.go:DocumentService.updateDocumentNameOnly":         "Explicit isArchiveDocument early return preserves ZIP source titles before any UpdateChunks.",
	"service/document/document_parse.go:DocumentService.deleteDocumentChunkImages":               "Lifecycle image cleanup projects only image keys; ZIP emits none and cleanup must include stale rows.",
	"service/document/knowledge_compile_status.go:DocumentService.documentKnowledgeCompileTypes": "Projects only compilation kind metadata for cleanup routing; ZIP has no compiler products.",
	"service/document/wiki_refresh.go:DocumentService.loadAvailabilityToggleChunkIDs":            "Called only after updateDocumentChunkAvailability handles archive generation in its separate guarded branch.",
	"service/document/wiki_refresh.go:DocumentService.loadSourceChunkIDs":                        "Lifecycle cleanup inventory of IDs only; must include unpublished generations for deletion.",
	"service/document/wiki_refresh.go:DocumentService.deleteSourceChunks":                        "Lifecycle purge of IDs from loadSourceChunkIDs; no content outlet, must remove stale rows.",
	"service/document/wiki_refresh.go:DocumentService.deleteDocumentGeneratedChunks":             "Reparse cleanup deletes hidden rows after task settlement; publication filtering would retain stale rows.",
	"service/file/file_commit.go:FileCommitService.readPageContent":                              "Reads trusted stored Wiki commit locations, not arbitrary source chunk IDs; fallback is wiki_page-only.",
	"service/file/file_commit.go:FileCommitService.readCurrentPageContent":                       "Requires wiki_page role, page type and slug; hydrates only the resolved page ID.",
	"service/graph/pipeline.go:Pipeline.searchEntities":                                          "Requires knowledge_graph_kwd=entity via KG search request.",
	"service/graph/pipeline.go:Pipeline.searchEntityTypes":                                       "Requires knowledge_graph_kwd=entity; projects type names only.",
	"service/graph/pipeline.go:Pipeline.searchRelations":                                         "Requires knowledge_graph_kwd=relation.",
	"service/graph/retrieval.go:searchTypeSamples":                                               "Requires knowledge_graph_kwd=ty2ents.",
	"service/graph/retrieval.go:searchCommunityContent":                                          "Requires knowledge_graph_kwd=community_report.",
	"service/graph/search.go:SearchEntities":                                                     "buildEntitySearchRequest pins knowledge_graph_kwd=entity.",
	"service/graph/search.go:SearchEntitiesByTypes":                                              "buildEntityTypeSearchRequest pins knowledge_graph_kwd=entity.",
	"service/graph/search.go:SearchRelations":                                                    "buildRelationSearchRequest pins knowledge_graph_kwd=relation.",
	"service/graph/search.go:SearchCommunityReports":                                             "buildCommunitySearchRequest pins knowledge_graph_kwd=community_report.",
	"service/graph/search.go:SearchTypeSamples":                                                  "buildTypeSamplesSearchRequest pins knowledge_graph_kwd=ty2ents.",
	"service/memory.go:MemoryService.ForgetMessage":                                              "MemoryIndexName selects separate memory message index, not document chunks.",
	"service/memory.go:MemoryService.UpdateMessageStatus":                                        "Get/update uses MemoryIndexName and memory ID.",
	"service/memory.go:MemoryService.GetMessageContent":                                          "Reads separate MemoryIndexName message store.",
	"service/memory.go:MemoryService.queryMessage":                                               "memorySearchIndexNames selects only memory stores.",
	"service/memory.go:MemoryService.getRecentMessage":                                           "memorySearchIndexNames selects only memory stores.",
	"service/memory.go:MemoryService.listMemoryMessages":                                         "Both raw and extracted searches target MemoryIndexName.",
	"service/nlp/datasetnav.go:NavService.navSearch":                                             "Private navigation executor; callers supply nav_doc/nav_cluster role filters or resolved navigation IDs.",
	"service/nlp/datasetnav.go:NavService.UpsertDoc":                                             "Update filter pins type_kwd=nav_doc, not source chunks.",
	"service/nlp/datasetnav.go:NavService.cleanupEmptyCluster":                                   "Deletes resolved or deterministic nav-cluster ID only.",
	"service/nlp/datasetnav.go:NavService.maybeSplitCluster":                                     "Validates child nav roles; rehomes their IDs and removes resolved nav-cluster row.",
	"service/nlp/datasetnav.go:NavService.mergeClusterDescription":                               "navClusterCondition pins cluster role and identity.",
	"service/nlp/datasetnav.go:NavService.appendDocToCluster":                                    "navClusterCondition pins cluster role and identity.",
	"service/nlp/datasetnav.go:NavService.deleteNavDoc":                                          "Deletes only IDs resolved by navigation-role search.",
	"service/nlp/datasetnav.go:NavService.findClusterContainingDoc":                              "Requires nav_cluster role and document membership; reads cluster metadata.",
	"service/nlp/datasetnav.go:NavService.removeDocFromCluster":                                  "navClusterCondition pins cluster role and identity.",
	"service/nlp/retrieval.go:RetrievalService.searchDenseOnly":                                  "Private retry invoked exclusively by Search with its already publication-filtered request; Search checks returned rows.",
	"service/skill_search.go:SkillSearchService.keywordSearch":                                   "Caller passes SkillIndexName, a separate skill store.",
	"service/skill_search.go:SkillSearchService.vectorSearch":                                    "Caller passes SkillIndexName, a separate skill store.",
	"service/skill_search.go:SkillSearchService.hybridSearch":                                    "Caller passes SkillIndexName, a separate skill store.",
	"service/wikisearch/engine_service.go:engineWikiService.AvailableFor":                        "Requires type_kwd=wiki_page; ZIP source is not a Wiki page.",
	"service/wikisearch/engine_service.go:engineWikiService.QueryPages":                          "Requires type_kwd=wiki_page; raw evidence backfill is guarded separately.",
	"syncer/connector/imap.go:realIMAPClient.Search":                                             "IMAP client Search method, not a DocEngine call.",
}

func TestArchiveEngineAccessArchitecture(t *testing.T) {
	root := filepath.Clean("../..")
	used := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(root, "engine") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		publicationAlias := "archivepublication"
		sortAlias := "sort"
		for _, imp := range file.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			if imp.Name != nil {
				if name == "ragflow/internal/service/archivepublication" {
					publicationAlias = imp.Name.Name
				}
				if name == "sort" {
					sortAlias = imp.Name.Name
				}
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil {
				r := fn.Recv.List[0].Type
				if star, ok := r.(*ast.StarExpr); ok {
					r = star.X
				}
				if id, ok := r.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			key := filepath.ToSlash(rel) + ":" + name
			access, guarded := archiveEngineAccess(fn.Body, publicationAlias, sortAlias)
			if !access {
				continue
			}
			if why, ok := archiveEngineExemptions[key]; ok {
				used[key] = true
				if strings.TrimSpace(why) == "" {
					t.Errorf("empty exemption: %s", key)
				}
				continue
			}
			if !guarded {
				t.Errorf("unguarded DocEngine-shaped access: %s", key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range archiveEngineExemptions {
		if !used[key] {
			t.Errorf("stale exemption: %s", key)
		}
	}
}

// Conservatively inspect every selector with a DocEngine operation signature,
// including interface aliases and closures; receiver-name heuristics would miss
// a new field name. Non-DocEngine lookalikes require an explicit reviewed entry.
func archiveEngineAccess(body *ast.BlockStmt, publicationAlias, sortAlias string) (access, guarded bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok {
			if pkg.Name == publicationAlias {
				name := sel.Sel.Name
				guarded = guarded || strings.HasPrefix(name, "Filter") || strings.HasPrefix(name, "Visible") || strings.HasPrefix(name, "Check") || name == "Publish" || name == "WithPublished" || name == "DeleteDocuments" || name == "DeleteDataset" || name == "Cleanup"
			}
			if pkg.Name == sortAlias {
				return true
			}
		}
		arity := map[string]int{"Search": 2, "GetChunk": 4, "UpdateChunks": 5, "DeleteChunks": 4}
		if want, ok := arity[sel.Sel.Name]; ok && len(call.Args) == want {
			access = true
		}
		return true
	})
	return
}

func TestArchiveEngineGuardDetectsNewPath(t *testing.T) {
	for _, tc := range []struct {
		body            string
		access, guarded bool
	}{
		{`return renamed.Search(ctx, req)`, true, false},
		{`return holder.store.GetChunk(ctx, index, id, kb)`, true, false},
		{`f := func() { x.DeleteChunks(ctx, cond, index, kb) }; f()`, true, false},
		{`ap.Filter(ctx, db, kb, docs, cond); return e.UpdateChunks(ctx, cond, data, index, kb)`, true, true},
		{`ap.CopySourceFields(out, row); return e.Search(ctx, req)`, true, false},
		{`sort.Search(n, predicate)`, false, false},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "probe.go", "package p; func probe() {"+tc.body+"}", 0)
		if err != nil {
			t.Fatal(err)
		}
		a, g := archiveEngineAccess(file.Decls[0].(*ast.FuncDecl).Body, "ap", "sort")
		if a != tc.access || g != tc.guarded {
			t.Fatalf("%s: %v %v", tc.body, a, g)
		}
	}
}
